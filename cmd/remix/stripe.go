package main

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

	"github.com/stripe/stripe-go/v82"
	"golang.org/x/time/rate"
)

// deleteFromStripe simulates deleting an object from Stripe based on the searchKey and searchValue.
func (s Stripe) deleteFromStripe(endpoint string, object Object, searchKey, searchValue string) error {
	if searchKey == "" || searchValue == "" {
		return fmt.Errorf("deleteFromStripe: searchKey and searchValue must be provided")
	}

	baseURL := "https://api.stripe.com/v1"
	deleteUrl := fmt.Sprintf("%s/%s/%s", baseURL, endpoint, searchValue)
	req, err := http.NewRequest("DELETE", deleteUrl, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	fmt.Printf("Making DELETE request to Stripe: %s\n", deleteUrl)
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

type Stripe struct {
	client           *stripe.Client
	apiKey           string
	app              *application
	limiter          *rate.Limiter
	systemInfo       SystemInfo
	duplicateChecker map[string][]ExpiringObject
}

func (app *application) newStripe(systemInfo SystemInfo, duplicateChecker map[string][]ExpiringObject) (system SystemInterface, err error) {

	if systemInfo.UseCliListener {

		if _, err := exec.LookPath("stripe"); err != nil {
			return nil, fmt.Errorf("Stripe CLI not found in PATH. Please install it to use Stripe listening mode: %w", err)
		}

		// Forward Stripe events to our local endpoint
		forwardURL := fmt.Sprintf("http://localhost:%d/%v", app.config.port, systemInfo.Name)
		cmd := exec.Command("stripe", "listen", "--forward-to", forwardURL)
		cmd.Stderr = os.Stderr

		cmd.Env = append(os.Environ(), fmt.Sprintf("STRIPE_API_KEY=%s", systemInfo.ApiKey))

		go func() {
			app.logger.Info("Starting Stripe CLI listener", "command", cmd.String())
			err := cmd.Run()
			if err != nil {
				return
			}
		}()
	}

	stripeClient := stripe.NewClient(systemInfo.ApiKey)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	listParams := &stripe.CouponListParams{}
	listParams.Limit = stripe.Int64(1)

	for _, err := range stripeClient.V1Coupons.List(ctx, listParams) {
		if err != nil {
			return nil, err
		}

		break
	}

	app.logger.Info("Stripe test api call was successful", "system", systemInfo.Name)

	stripeSystem := &Stripe{
		client:           stripeClient,
		app:              app,
		limiter:          rate.NewLimiter(rate.Limit(systemInfo.RateLimit), systemInfo.RateBucketSize),
		systemInfo:       systemInfo,
		duplicateChecker: duplicateChecker,
		apiKey:           systemInfo.ApiKey,
	}

	app.storageEngine.setSafeIndexMap(systemInfo.Name, 0)

	go stripeSystem.watchQueue()

	return stripeSystem, nil
}

func (s Stripe) watchQueue() {
	var index int64
	for {
		// Get the last safe object index for this system
		var exists bool
		index, exists = s.app.storageEngine.getSafeIndexMap(s.systemInfo.Name)
		if !exists {
			panic(fmt.Sprintf("safe index not found for system %s", s.systemInfo.Name))
		}

		// Wait for rate limiter
		err := s.limiter.Wait(context.Background())
		if err != nil {
			// Optionally log or handle error, then break or continue
			continue
		}

		// Query safeObjects after lastIndex
		objects := s.app.storageEngine.getSafeObjectsFromIndex(index)
		if len(objects) > 0 {
			// Process new objects as needed
			index += int64(len(objects))
		}

		for _, object := range objects {

			var searchKey string
			var searchValue string

			for locationInSystem, pushLocation := range s.systemInfo.PushRouter[object.Type] {
				newObj := Object{
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

					if field.Hardcode != nil && !isZeroValue(field.Hardcode) {
						newObj.Payload[field.Field] = field.Hardcode
					}
				}

				var objectIsDuplicate bool
				foundDuplicate := false
				for i, expiringObj := range s.duplicateChecker[object.Type] {
					objectIsDuplicate = true
					for k, v := range expiringObj.Object.Payload {
						if v != object.Payload[k] {
							objectIsDuplicate = false
							break
						}
					}
					if objectIsDuplicate {
						// If we found a duplicate, we can remove it from the duplicate checker
						s.duplicateChecker[object.Type] = append(s.duplicateChecker[object.Type][:i], s.duplicateChecker[object.Type][i+1:]...)
						foundDuplicate = true
						break
					}
				}

				if !foundDuplicate {
					switch object.Operation {
					case "upsert":
						// Upsert the object to Stripe
						_, err := s.upsertObject(locationInSystem, newObj, object.Type, searchKey, searchValue)
						if err != nil {
							s.app.logger.Error("Failed to upsert object to Stripe", "error", err, "object", newObj)
							continue
						}
					case "delete":
						err := s.deleteFromStripe(locationInSystem, newObj, searchKey, searchValue)
						if err != nil {
							s.app.logger.Error("Failed to delete object from Stripe", "error", err, "object", newObj)
							continue
						}
					}
				}
			}
		}

		// Update the safe index map for this system
		s.app.storageEngine.setSafeIndexMap(s.systemInfo.Name, index)
	}
}

func (s Stripe) upsertObject(endpoint string, object Object, objectType string, searchKey, searchValue string) ([]byte, error) {
	// Replace with your actual secret key or use an environment variable
	form := url.Values{}

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

		expiringObj := newExpiringObject(object, s.app.config.keepDuplicatesFor)
		s.duplicateChecker[objectType] = append(s.duplicateChecker[objectType], expiringObj)
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

	expiringObj := newExpiringObject(object, s.app.config.keepDuplicatesFor)
	s.duplicateChecker[objectType] = append(s.duplicateChecker[objectType], expiringObj)

	return body, nil
}

func (s *Stripe) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// Immediately acknowledge receipt to Stripe
	w.WriteHeader(http.StatusOK)
	var err error

	var event stripe.Event
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		s.app.logger.Error("Failed to decode Stripe event", "error", err)
		return
	}

	objectName := string(event.Type)
	var operationType string
	// If the event type contains a period, we only want the part before it
	if idx := indexOfPeriod(objectName); idx > 0 {
		operationType = objectName[idx+1:]
		objectName = objectName[:idx]
	}

	switch operationType {
	case "created", "updated":
		operationType = "upsert"
	case "deleted":
		operationType = "delete"
	default:
		s.app.logger.Error("Unknown Stripe event type", "event_type", event.Type)
		return
	}

	var obj map[string]any
	err = json.Unmarshal(event.Data.Raw, &obj)
	if err != nil {
		s.app.logger.Error("Failed to unmarshal Stripe event data", "error", err)
		return
	}

	newObjs := make(map[string]map[string]any)

	for schemaName, fields := range s.systemInfo.ReceiveRouter[objectName] {
		newModel := map[string]any{}

		for keyInObj, field := range fields {
			if field.Hardcode != nil && !isZeroValue(field.Hardcode) {
				newModel[field.Field] = field.Hardcode
			} else {
				newModel[field.Field] = getNestedValue(obj, keyInObj)
			}
		}

		newObjs[schemaName] = newModel
	}

	for schemaName, obj := range newObjs {

		schema, inMap := s.app.schemaMap[schemaName]
		if !inMap {
			fmt.Printf("No schema found for object: %s\n", objectName)
			return
		}

		for k, v := range obj {
			if v == nil {
				delete(obj, k)
			}
		}

		err = schema.Validate(obj)
		if err != nil {
			fmt.Printf("Object failed stripe schema validation for '%s': %v\n", objectName, err)
			return
		}

		var objectIsDuplicate bool
		foundDuplicate := false
		for i, expiringObj := range s.duplicateChecker[schemaName] {

			objectIsDuplicate = true

			for k, v := range obj {
				if v != expiringObj.Object.Payload[k] {
					objectIsDuplicate = false
					break
				}
			}

			if objectIsDuplicate {
				// If we found a duplicate, we can remove it from the duplicate checker
				s.duplicateChecker[schemaName] = append(s.duplicateChecker[schemaName][:i], s.duplicateChecker[schemaName][i+1:]...)
				foundDuplicate = true
				break
			}
		}

		if !foundDuplicate {

			object := Object{
				Type:      schemaName,
				Operation: operationType,
				Payload:   obj,
			}
			s.app.storageEngine.addSafeObject(object)
			expiringObj := newExpiringObject(object, s.app.config.keepDuplicatesFor)
			s.duplicateChecker[schemaName] = append(s.duplicateChecker[schemaName], expiringObj)
		}
	}
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
