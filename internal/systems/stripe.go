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
	"sync"
	"time"

	"github.com/sqlpipe/remix/internal/app"
	"github.com/sqlpipe/remix/internal/helpers"
	"github.com/stripe/stripe-go/v82"
	"golang.org/x/time/rate"
)

type Stripe struct {
	client           *stripe.Client
	apiKey           string
	limiter          *rate.Limiter
	systemInfo       SystemInfo
	seenObjects      map[string][]*app.Object
	seenObjectsMutex sync.RWMutex
}

func newStripe(systemInfo SystemInfo) (system SystemInterface, err error) {
	err = startStripeCliListener(systemInfo)
	if err != nil {
		return nil, err
	}

	stripeClient, err := newStripeClientWithHealthCheck(systemInfo.ApiKey)
	if err != nil {
		return nil, err
	}

	stripeSystem := &Stripe{
		client:      stripeClient,
		limiter:     rate.NewLimiter(rate.Limit(systemInfo.RateLimit), systemInfo.RateBucketSize),
		systemInfo:  systemInfo,
		seenObjects: make(map[string][]*app.Object),
		apiKey:      systemInfo.ApiKey,
	}

	for schemaName := range app.SchemaMap {
		stripeSystem.seenObjects[schemaName] = make([]*app.Object, 0)
	}

	go stripeSystem.watchQueue()

	return stripeSystem, nil
}

// Helper to parse Stripe event type into objectName and operationType
func parseStripeEventType(eventType string) (objectName, operationType string, err error) {
	objectName = eventType
	if idx := indexOfPeriod(objectName); idx > 0 {
		operationType = objectName[idx+1:]
		objectName = objectName[:idx]
	} else {
		err = fmt.Errorf("event type missing period: %s", eventType)
		return
	}

	switch operationType {
	case "created", "updated":
		operationType = "upsert"
	case "deleted":
		operationType = "delete"
	default:
		err = fmt.Errorf("unknown Stripe event type: %s", eventType)
	}
	return
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

	var webhookData map[string]any
	err := json.Unmarshal(event.Data.Raw, &webhookData)
	if err != nil {
		app.Logger.Error("Failed to unmarshal Stripe event data", "error", err)
		return
	}

	if app.Config.LogLevel == "debug" {
		app.AddToDebugStore(app.DebugMessage{Payload: webhookData, Operation: "Received webhook", System: s.systemInfo.Name})
	}

	newObjs := applyReceiveMixer(objectName, operationType, webhookData, s.systemInfo.ReceiveMixer)

	for schemaName, obj := range newObjs {

		// Validate the object using schema and remove nil values
		err = validateObject(schemaName, obj, objectName)
		if err != nil {
			return
		}

		if s.checkIfSeen(schemaName, obj) {
			if app.Config.LogLevel == "debug" {
				app.AddToDebugStore(app.DebugMessage{Payload: obj, Operation: "Inbound duplicate", System: s.systemInfo.Name})
			}
			continue
		}

		if app.Config.LogLevel == "debug" {
			app.AddToDebugStore(app.DebugMessage{Payload: obj, Operation: "Adding to queue", System: s.systemInfo.Name})
		}

		app.ObjectStore.AddSafeObject(obj)
		s.duplicateChecker[schemaName] = append(s.duplicateChecker[schemaName], &obj)
	}
}

// watchQueue is the main loop for processing objects from the queue and applying them to Stripe.
func (s *Stripe) watchQueue() {
	var index int64
	for {
		if err := s.limiter.Wait(context.Background()); err != nil {
			continue
		}
		var exists bool
		index, exists = app.ObjectStore.GetSafeIndexMap(s.systemInfo.Name)
		if !exists {
			panic(fmt.Sprintf("safe index not found for system %s", s.systemInfo.Name))
		}
		s.processPushObjects(&index)
		app.ObjectStore.SetSafeIndexMap(s.systemInfo.Name, index)
	}
}

// startStripeCliListener starts the Stripe CLI listener if UseCliListener is enabled.
func startStripeCliListener(systemInfo SystemInfo) error {
	if !systemInfo.UseCliListener {
		return nil
	}

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
			return
		}
	}()
	return nil
}

// newStripeClientWithHealthCheck creates a new Stripe client and performs a health check by listing coupons.
func newStripeClientWithHealthCheck(apiKey string) (*stripe.Client, error) {
	stripeClient := stripe.NewClient(apiKey)

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

	return stripeClient, nil
}

// processPushObjects processes safe objects from the ObjectStore and applies them to Stripe.
func (s *Stripe) processPushObjects(index *int64) {
	objects := app.ObjectStore.GetSafeObjectsFromIndex(*index)
	if len(objects) > 0 {
		*index += int64(len(objects))
		if app.Config.LogLevel == "debug" {
			app.AddToDebugStore(app.DebugMessage{Payload: objects, Operation: "Got from queue", System: s.systemInfo.Name})
		}
	}

	for _, object := range objects {
		var searchKey, searchValue string
		for locationInSystem, pushLocation := range s.systemInfo.PushMixer[object.Type] {
			newObj := app.Object{
				Payload:   make(map[string]any),
				Operation: object.Operation,
				Type:      object.Type,
			}
			for keyInSchema, field := range pushLocation {
				if _, ok := object.Payload[keyInSchema]; ok {
					newObj.Payload[field.Field] = object.Payload[keyInSchema]
					if field.SearchKey {
						searchKey = field.Field
						searchValue = fmt.Sprint(newObj.Payload[field.Field])
					}
				}
				if field.Hardcode != nil && !helpers.IsZeroValue(field.Hardcode) {
					newObj.Payload[field.Field] = field.Hardcode
				}
			}

			if !s.checkIfSeen(object.Type, newObj) {
				s.processStripeOperation(locationInSystem, newObj, searchKey, searchValue)
			}
		}
	}
}

// processStripeOperation dispatches upsert or delete operations to Stripe.
func (s *Stripe) processStripeOperation(locationInSystem string, object app.Object, searchKey, searchValue string) {
	switch object.Operation {
	case "upsert":
		_, err := s.upsertObject(locationInSystem, object, object.Type, searchKey, searchValue)
		if err != nil {
			app.Logger.Error("Failed to upsert object to Stripe", "error", err, "object", object)
		}
	case "delete":
		err := s.deleteFromStripe(locationInSystem, searchKey, searchValue)
		if err != nil {
			app.Logger.Error("Failed to delete object from Stripe", "error", err, "object", object)
		}
	}
}

// processStripeOperation dispatches upsert or delete operations to Stripe.
func (s Stripe) upsertObject(endpoint string, object app.Object, objectType string, searchKey, searchValue string) ([]byte, error) {
	// Replace with your actual secret key or use an environment variable
	form := url.Values{}

	if app.Config.LogLevel == "debug" {
		app.AddToDebugStore(app.DebugMessage{Payload: object, Operation: "Upserting into", System: s.systemInfo.Name})
	}

	for key, value := range object.Payload {

		if key == searchKey {
			continue
		}

		switch v := value.(type) {
		case string:
			form.Set(key, v)
		case int, int64, float64:
			form.Set(key, fmt.Sprintf("%v", v))
		case bool:
			if v {
				form.Set(key, "true")
			} else {
				form.Set(key, "false")
			}
		default:
			return nil, fmt.Errorf("unsupported value type for key %s: %T", key, value)
		}
	}

	if len(form) == 0 {
		return nil, nil
	}

	baseURL := "https://api.stripe.com/v1"
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
			Type:      objectType,
			Operation: object.Operation,
			Payload:   object.Payload,
		}
		s.duplicateChecker[objectType] = append(s.duplicateChecker[objectType], newObject)
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
		Type:      objectType,
		Operation: object.Operation,
		Payload:   object.Payload,
	}
	s.duplicateChecker[objectType] = append(s.duplicateChecker[objectType], newObject)

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

// checkInboundObjectForDuplicate checks if the inbound object is a duplicate and removes it if found.
func (s *Stripe) checkIfSeen(schemaName string, obj app.Object) bool {
	for i := len(s.duplicateChecker[schemaName]) - 1; i >= 0; i-- {
		seenObject := s.duplicateChecker[schemaName][i]
		for k, v := range obj.Payload {
			if v != seenObject.Payload[k] {
				break
			}
		}
	}

	return false
}

// func (s *Stripe) checkOutboundObjectForDuplicate(schemaName string, obj app.Object) bool {
// 	var objectIsDuplicate bool
// 	for i, seenObject := range s.duplicateChecker[schemaName] {
// 		objectIsDuplicate = true
// 		for k, v := range obj.Payload {
// 			if v != seenObject.Payload[k] {
// 				objectIsDuplicate = false
// 				break
// 			}
// 		}
// 		if objectIsDuplicate {
// 			s.duplicateChecker[schemaName] = append(s.duplicateChecker[schemaName][:i], s.duplicateChecker[schemaName][i+1:]...)
// 			return true
// 		}
// 	}
// 	return false
// }
