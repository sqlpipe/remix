package systems

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/sqlpipe/remix/internal/app"
	"github.com/stripe/stripe-go/v82"
	"golang.org/x/time/rate"
)

type Stripe struct {
	client     *stripe.Client
	apiKey     string
	limiter    *rate.Limiter
	systemInfo *SystemInfo
	baseURL    string
}

func newStripe(systemInfo *SystemInfo) (system SystemInterface, err error) {
	if systemInfo.UseCliListener {
		err = startStripeCliListener(systemInfo)
		if err != nil {
			return nil, err
		}
	}

	stripeStruct, err := createStripeStruct(systemInfo)
	if err != nil {
		return nil, err
	}

	go stripeStruct.loop()

	return stripeStruct, nil
}

// Helper to parse Stripe event type into objectName and operationType
func parseStripeEventType(eventType string) (string, string, error) {
	var objectName string
	var operationType string

	idx := indexOfPeriod(objectName)
	if idx > 0 {
		objectName = objectName[:idx]
		operationType = objectName[idx+1:]
	} else {
		return "", "", fmt.Errorf("event type missing period: %s", eventType)
	}

	switch operationType {
	case "created", "updated":
		operationType = "upsert"
	case "deleted":
		operationType = "delete"
	default:
		return "", "", fmt.Errorf("unknown Stripe event type: %s", eventType)
	}

	return objectName, operationType, nil
}

func (s *Stripe) HandleWebhook(w http.ResponseWriter, r *http.Request) {

	// Immediately acknowledge receipt to Stripe
	w.WriteHeader(http.StatusOK)

	var event stripe.Event
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		app.Logger.Error("Failed to decode Stripe event", "error", err)
		return
	}

	objectName, operationType, parseErr := parseStripeEventType(string(event.Type))
	if parseErr != nil {
		app.Logger.Error(parseErr.Error(), "event_type", event.Type)
		return
	}

	var payload map[string]any
	err := json.Unmarshal(event.Data.Raw, &payload)
	if err != nil {
		app.Logger.Error("Failed to unmarshal Stripe event data", "error", err)
		return
	}

	if app.Config.LogLevel == "debug" {
		app.AddToDebugStore(app.DebugMessage{Payload: payload, Operation: "Received webhook", System: s.systemInfo.Name})
	}

	incomingObject := &app.Object{
		Schema:    objectName,
		Operation: operationType,
		Payload:   payload,
	}

	incomingObjects := applyReceiveMixer(incomingObject, s.systemInfo.ReceiveMixer, objectName)
	if app.Config.LogLevel == "debug" {
		app.AddToDebugStore(app.DebugMessage{Payload: incomingObjects, Operation: "Created incoming objects from webhook", System: s.systemInfo.Name})
	}

	for schemaName, incomingObject := range incomingObjects {

		schema, inMap := app.SchemaMap[schemaName]
		if !inMap {
			app.Logger.Error("no schema found for object", "object", incomingObject.Schema, "system", s.systemInfo.Name)
			return
		}

		err = schema.Validator.Validate(incomingObject)
		if err != nil {
			app.Logger.Error("object failed validation", "object", incomingObject.Schema, "system", s.systemInfo.Name, "error", err)
			return
		}

		foundDuplicate := app.DuplicateChecker.CheckIfSeen(incomingObject)
		if !foundDuplicate {
			app.ObjectQueue.AddSafeObject(incomingObject, s.systemInfo.Name)
		}
	}
}

// the main loop for processing objects from the queue and applying them to Stripe.
func (s *Stripe) loop() {
	var index int64
	for {
		err := s.limiter.Wait(context.Background())
		if err != nil {
			app.Logger.Error("error waiting for rate limiter", "error", err, "system", s.systemInfo.Name)
			continue
		}
		var exists bool
		index, exists = app.ObjectQueue.GetSafeIndex(s.systemInfo.Name)
		if !exists {
			app.Logger.Error("safe index not found for system", "system", s.systemInfo.Name)
			os.Exit(1)
		}

		if s.systemInfo.PushMixer != nil {
			index, err = s.processQueue(index)
			if err != nil {
				app.Logger.Error("errorin queue processing", "error", err, "system", s.systemInfo.Name)
				continue
			}
		}

		app.ObjectQueue.SetSafeIndex(s.systemInfo.Name, index)
	}
}

// startStripeCliListener starts the Stripe CLI listener if UseCliListener is enabled.
func startStripeCliListener(systemInfo *SystemInfo) error {
	if _, err := exec.LookPath("stripe"); err != nil {
		return fmt.Errorf("Stripe CLI not found in PATH. Please install it to use Stripe listening mode: %w", err)
	}

	// Forward Stripe events to our local endpoint
	forwardURL := fmt.Sprintf("http://localhost:%d/%v", app.Config.Port, systemInfo.Name)
	cmd := exec.Command("stripe", "listen", "--forward-to", forwardURL)
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), fmt.Sprintf("STRIPE_API_KEY=%s", systemInfo.ApiKey))

	go func() {
		app.Logger.Info("Starting Stripe CLI listener", "command", cmd.String())
		err := cmd.Run()
		if err != nil {
			app.Logger.Error("stripe CLI listener failed", "error", err)
			os.Exit(1)
		}
	}()

	return nil
}

// newStripeClient creates a new Stripe client and performs a health check by listing coupons.
func createStripeStruct(systemInfo *SystemInfo) (*Stripe, error) {

	stripeClient := stripe.NewClient(systemInfo.ApiKey)

	listParams := &stripe.CouponListParams{}
	listParams.Limit = stripe.Int64(1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	for _, err := range stripeClient.V1Coupons.List(ctx, listParams) {
		if err != nil {
			return nil, err
		}
		break
	}

	stripeStruct := &Stripe{
		client:     stripeClient,
		limiter:    rate.NewLimiter(rate.Limit(systemInfo.RateLimit), systemInfo.RateBucketSize),
		systemInfo: systemInfo,
		baseURL:    "https://api.stripe.com/v1",
	}

	return stripeStruct, nil
}

// processPushObjects processes safe objects from the ObjectQueue and applies them to PostgreSQL.
func (s *Stripe) processQueue(index int64) (int64, error) {

	objects := app.ObjectQueue.GetSafeObjectsFromIndex(index, s.systemInfo.Name)

	for _, object := range objects {
		objectsToPush := applyPushMixer(object, s.systemInfo.PushMixer)
		for _, pushObject := range objectsToPush {
			foundDuplicate := app.DuplicateChecker.CheckIfSeen(pushObject)
			if !foundDuplicate {
				switch pushObject.Operation {
				case "upsert":
					err := s.upsertObject(pushObject)
					if err != nil {
						return index, fmt.Errorf("error upserting to Stripe: %v", err)
					}
				case "delete":
					err := s.deleteObject(pushObject)
					if err != nil {
						return index, fmt.Errorf("error deleting from Stripe: %v", err)
					}
				}

				app.DuplicateChecker.AddObject(object)
			}
		}
	}

	if len(objects) > 0 {
		index += int64(len(objects))
		app.ObjectQueue.SetSafeIndex(s.systemInfo.Name, index)
	}

	return index, nil
}

// processStripeOperation dispatches upsert or delete operations to Stripe.
func (s Stripe) upsertObject(object *app.Object) error {
	for locationInSystem := range (*s.systemInfo.PushMixer)[object.Schema] {

		presentSearchKeys := []string{}
		for _, field := range (*s.systemInfo.PushMixer)[object.Schema][locationInSystem].SearchKeys {
			_, ok := object.Payload[field]
			if ok {
				presentSearchKeys = append(presentSearchKeys, field)
			}
		}

		form := url.Values{}
		for key, value := range object.Payload {
			form.Set(key, fmt.Sprintf("%v", value))
		}

		if len(form) == 0 {
			return fmt.Errorf("no form data to upsert")
		}

		encoded := form.Encode()

		// If searchValue is empty, just create (insert) the object
		if searchValue == "" {
			createUrl := fmt.Sprintf("%s/%s", baseURL, endpoint)
			req, err := http.NewRequest("POST", createUrl, bytes.NewBufferString(encoded))
			if err != nil {
				return nil, err
			}

			req.Header.Set("Authorization", "Bearer "+s.apiKey)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, fmt.Errorf("failed to create object at stripe route %s, error making request: %w", createUrl, err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, fmt.Errorf("failed to create object at stripe route %s, error reading response body: %w", createUrl, err)
			}

			if resp.StatusCode >= 400 {
				return nil, fmt.Errorf("failed to create object at stripe route %s, status code: %d, response: %s", createUrl, resp.StatusCode, string(body))
			}

			newObject := &app.Object{
				Schema:    objectSchema,
				Operation: object.Operation,
				Payload:   object.Payload,
			}
			app.DuplicateChecker.AddObject(newObject)
			return body, nil
		}

		// Otherwise, update it
		updateUrl := fmt.Sprintf("%s/%s/%s", baseURL, endpoint, searchValue)
		req, err := http.NewRequest("POST", updateUrl, bytes.NewBufferString(encoded))
		if err != nil {
			return nil, err
		}

		req.Header.Set("Authorization", "Bearer "+s.apiKey)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		if app.Config.LogLevel == "debug" {
			app.AddToDebugStore(app.DebugMessage{Payload: object.Payload, Operation: fmt.Sprintf("Upserting into %v", locationInSystem), System: p.systemInfo.Name})
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body from stripe route %s, error reading response body: %w", updateUrl, err)
		}

		newObject := &app.Object{
			Schema:    objectSchema,
			Operation: object.Operation,
			Payload:   object.Payload,
		}
		app.DuplicateChecker.AddObject(newObject)
	}

	return body, nil
}

// indexOfPeriod returns the index of the first period in s, or -1 if not found
func indexOfPeriod(s string) int {
	for i, c := range s {
		if c == '.' {
			return i
		}
	}
	return -1
}

// deleteFromStripe simulates deleting an object from Stripe based on the searchKey and searchValue.
func (s Stripe) deleteFromStripe(endpoint string, searchKey, searchValue string) error {
	if searchKey == "" || searchValue == "" {
		return fmt.Errorf("deleteFromStripe: searchKey and searchValue must be provided")
	}

	if app.Config.LogLevel == "debug" {
		app.AddToDebugStore(app.DebugMessage{Payload: searchValue, Operation: "Deleting from", System: s.systemInfo.Name})
	}

	baseURL := "https://api.stripe.com/v1"
	deleteUrl := fmt.Sprintf("%s/%s/%s", baseURL, endpoint, searchValue)
	req, err := http.NewRequest("DELETE", deleteUrl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to delete object at stripe route %s, error making request: %w", deleteUrl, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body from stripe route %s, error reading response body: %w", deleteUrl, err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to delete object at stripe route %s, status code: %d, response: %s", deleteUrl, resp.StatusCode, string(body))
	}

	return nil
}
