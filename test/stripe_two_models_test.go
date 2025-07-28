package test

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

func createPostgresqlProductsAndPricesTables(t *testing.T, db *sql.DB) {
	_, err := db.Exec(`
			   CREATE TABLE products (
					   id TEXT PRIMARY KEY,
					   product_name TEXT NOT NULL,
					   default_price_id TEXT UNIQUE,
					   active BOOLEAN DEFAULT TRUE,
					   created TIMESTAMPTZ,
					   updated TIMESTAMPTZ,
					   description TEXT,
					   livemode BOOLEAN DEFAULT FALSE,
					   statement_descriptor TEXT,
					   unit_label TEXT,
					   category TEXT,
					   internal_notes TEXT
			   );
	   `)
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}

	_, err = db.Exec(`
			   CREATE TABLE prices (
					   id text PRIMARY KEY,
					   product_id TEXT NOT NULL,
					   unit_amount INT NOT NULL,
					   currency TEXT NOT NULL
			   );
	   `)
	if err != nil {
		t.Fatalf("Failed to create prices table: %v", err)
	}
}

func TestTwoTablesStreaming(t *testing.T) {

	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Fatalf("Could not connect to docker: %s", err)
	}

	// --- CLEANUP AT START: Remove any existing resources ---
	removeIfExists := func(name string) {
		_ = pool.RemoveContainerByName(name)
	}
	removeNetworkIfExists := func(name string) {
		nets, err := pool.NetworksByName(name)
		if err == nil && len(nets) > 0 {
			_ = pool.RemoveNetwork(&nets[0])
		}
	}
	removeIfExists("remix")
	removeIfExists("test-postgres")
	removeNetworkIfExists("sqlpipe-test-network")
	// --- END CLEANUP AT START ---

	pool.MaxWait = 20 * time.Second

	postgresqlPassword := "Mypass123"
	postgresqlUsername := "postgres"
	postgresqlDatabase := "postgres"

	// Resource handles for cleanup
	var (
		network             *dockertest.Network
		postgresqlContainer *dockertest.Resource
		sqlpipeContainer    *dockertest.Resource
	)

	// REMOVE signal handler and defer cleanup logic

	// Create a network for both containers, only if it doesn't already exist
	networks, err := pool.NetworksByName("sqlpipe-test-network")
	if err != nil {
		t.Fatalf("Could not list docker networks: %s", err)
	}
	if len(networks) > 0 {
		network = &networks[0]
		log.Printf("Using existing docker network: %s", network.Network.Name)
	} else {
		network, err = pool.CreateNetwork("sqlpipe-test-network")
		if err != nil {
			t.Fatalf("Could not create docker network: %s", err)
		}
	}

	postgresqlContainer, err = pool.BuildAndRunWithOptions("./postgresql.dockerfile", &dockertest.RunOptions{
		Name: "test-postgres",
		Env: []string{
			fmt.Sprintf("POSTGRES_USER=%v", postgresqlUsername),
			fmt.Sprintf("POSTGRES_PASSWORD=%v", postgresqlPassword),
			fmt.Sprintf("POSTGRES_DB=%v", postgresqlDatabase),
		},
		NetworkID:    network.Network.ID,
		ExposedPorts: []string{"5432/tcp"},
		PortBindings: map[docker.Port][]docker.PortBinding{
			"5432/tcp": {{HostIP: "0.0.0.0", HostPort: "5432"}},
		},
		Cmd: []string{
			"postgres",
			"-c", "wal_level=logical",
			"-c", "max_replication_slots=5",
			"-c", "max_wal_senders=5",
			"-c", "max_connections=100",
		},
	})
	if err != nil {
		t.Fatalf("Could not start resource: %s", err)
	}

	var db *sql.DB
	if err := pool.Retry(func() error {
		var err error
		port := postgresqlContainer.GetPort("5432/tcp")
		dsn := fmt.Sprintf("postgres://%v:%v@localhost:%s/%v?sslmode=disable", postgresqlUsername, postgresqlPassword, port, postgresqlDatabase)
		db, err = sql.Open("pgx", dsn)
		if err != nil {
			return err
		}
		return db.Ping()
	}); err != nil {
		t.Fatalf("Could not connect to database: %s", err)
	}

	createPostgresqlProductsAndPricesTables(t, db)

	buildCmd := exec.Command("go", []string{"build", "-o", "../bin/remix", "../cmd/remix"}...)
	buildCmd.Env = append(os.Environ(),
		"GOOS=linux",
		fmt.Sprintf("GOARCH=%v", runtime.GOARCH),
		"CGO_ENABLED=0",
	)

	// buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		t.Fatalf("Failed to build remix app: %v", err)
	}

	systemsHostDir, err := filepath.Abs("./config/two-tables")
	if err != nil {
		t.Fatalf("Failed to get absolute path for systems config: %v", err)
	}
	if _, err := os.Stat(systemsHostDir); os.IsNotExist(err) {
		t.Fatalf("Systems config directory does not exist: %s", systemsHostDir)
	}

	modelsHostDir, err := filepath.Abs("./config/two-tables")
	if err != nil {
		t.Fatalf("Failed to get absolute path for models config: %v", err)
	}
	if _, err := os.Stat(modelsHostDir); os.IsNotExist(err) {
		t.Fatalf("Models config directory does not exist: %s", modelsHostDir)
	}

	sqlpipeContainer, err = pool.BuildAndRunWithOptions("../dockerfile", &dockertest.RunOptions{
		Name: "remix",
		Env: []string{
			"PORT=4000",
			"CONFIG_DIR=/config/two-tables",
			"LOG_LEVEL=debug",
		},
		Mounts: []string{
			fmt.Sprintf("%s:/config/two-tables/systems", systemsHostDir),
			fmt.Sprintf("%s:/config/two-tables/models", modelsHostDir),
		},
		NetworkID:    network.Network.ID,
		ExposedPorts: []string{"4000/tcp"},
		PortBindings: map[docker.Port][]docker.PortBinding{
			"4000/tcp": {{HostIP: "0.0.0.0", HostPort: "4000"}},
		},
	})
	if err != nil {
		t.Fatalf("Could not start resource: %s", err)
	}

	go func() {
		pool.Client.Logs(docker.LogsOptions{
			Container:    sqlpipeContainer.Container.ID,
			OutputStream: os.Stdout,
			ErrorStream:  os.Stderr,
			Follow:       true,
			Stdout:       true,
			Stderr:       true,
		})
	}()

	err = pool.Retry(func() error {

		inspect, err := pool.Client.InspectContainer(sqlpipeContainer.Container.ID)
		if err != nil {
			return fmt.Errorf("failed to inspect container: %w", err)
		}
		if !inspect.State.Running {
			return fmt.Errorf("container exited with code: %d", inspect.State.ExitCode)
		}

		hostPort := sqlpipeContainer.GetPort("4000/tcp")
		healthcheckURL := fmt.Sprintf("http://localhost:%s/v1/healthcheck", hostPort)

		resp, err := http.Get(healthcheckURL)
		if err != nil {
			return fmt.Errorf("healthcheck error: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("healthcheck returned status %d", resp.StatusCode)
		}
		return nil // success!
	})
	if err != nil {
		t.Fatalf("SQLpipe healthcheck failed: %v", err)
	}

	fmt.Println("SQLpipe is running and healthy!")

	time.Sleep(1 * time.Second)

	stripeCmd := exec.Command("stripe", "trigger", "product.created")
	stripeCmd.Stdout = os.Stdout
	stripeCmd.Stderr = os.Stderr
	fmt.Println("stripe api key: ", os.Getenv("STRIPE_API_KEY"))
	stripeCmd.Env = append(os.Environ(), fmt.Sprintf("STRIPE_API_KEY=%s", os.Getenv("STRIPE_API_KEY")))
	err = stripeCmd.Run()
	if err != nil {
		t.Fatalf("Failed to run stripe trigger: %v", err)
	}

	fmt.Println("Test is running. Press Ctrl+C to exit.")
	select {}
}
