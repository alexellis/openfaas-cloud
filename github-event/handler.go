package function

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/alexellis/hmac"
	"github.com/openfaas/openfaas-cloud/sdk"
)

// Source name for this function when auditing
const Source = "github-event"

// Handle receives events from the GitHub app and checks the origin via
// HMAC. Valid events are push or installation events.
func Handle(req []byte) string {
	eventHeader := os.Getenv("Http_X_Github_Event")
	xHubSignature := os.Getenv("Http_X_Hub_Signature")

	if eventHeader != "push" &&
		eventHeader != "installation_repositories" &&
		eventHeader != "integration_installation" &&
		eventHeader != "installation" {

		auditEvent := sdk.AuditEvent{
			Message: "bad event: " + eventHeader,
			Source:  Source,
		}

		sdk.PostAudit(auditEvent)

		return fmt.Sprintf("%s cannot handle event: %s", Source, eventHeader)
	}

	if sdk.ValidateCustomers() {
		customersURL := os.Getenv("customers_url")

		customers, getErr := getCustomers(customersURL)
		if getErr != nil {
			return getErr.Error()
		}

		customer := sdk.Customer{}
		unmarshalErr := json.Unmarshal(req, &customer)
		if unmarshalErr != nil {
			return fmt.Sprintf("Error while un-marshaling customers: %s", unmarshalErr.Error())
		}

		if !validCustomer(customers, customer.Sender.Login) {

			auditEvent := sdk.AuditEvent{
				Message: "Customer not found",
				Owner:   customer.Sender.Login,
				Source:  Source,
			}

			sdk.PostAudit(auditEvent)

			return fmt.Sprintf("Customer: %s not found in CUSTOMERS file via %s", customer.Sender.Login, customersURL)
		}
	}

	if eventHeader == "push" {
		headers := map[string]string{
			"X-Hub-Signature": xHubSignature,
			"X-GitHub-Event":  eventHeader,
			"Content-Type":    "application/json",
		}

		body, statusCode, err := forward(req, "github-push", headers)

		if statusCode == http.StatusOK {
			return fmt.Sprintf("Forwarded to function: %d, %s", statusCode, body)
		}

		if err != nil {
			return err.Error()
		}

		return body
	}

	if eventHeader == "installation" ||
		eventHeader == "installation_repositories" ||
		eventHeader == "integration_installation" {

		if sdk.HmacEnabled() {
			webhookSecretKey, secretErr := sdk.ReadSecret("github-webhook-secret")
			if secretErr != nil {
				return secretErr.Error()
			}

			validateErr := hmac.Validate(req, xHubSignature, webhookSecretKey)
			if validateErr != nil {
				log.Fatal(validateErr)
			}
		}

		event := InstallationRepositoriesEvent{}
		err := json.Unmarshal(req, &event)
		if err != nil {
			return err.Error()
		}

		fmt.Printf("event.Action: %s\n", event.Action)

		switch event.Action {
		case "created", "added":

			addedVal := ""
			if event.RepositoriesAdded != nil {
				for _, added := range event.RepositoriesAdded {
					addedVal += added.FullName + ", "
				}
			}
			if event.Repositories != nil {
				for _, added := range event.Repositories {
					addedVal += added.FullName + ", "
				}
			}

			auditEvent := sdk.AuditEvent{
				Message: event.Installation.Account.Login + " added repositories: " + addedVal,
				Source:  Source,
			}

			sdk.PostAudit(auditEvent)

		case "removed":
			garbageRequests := []GarbageRequest{}
			for _, repo := range event.RepositoriesRemoved {
				fmt.Printf("Need to remove: %s.\n", repo.FullName)

				garbageRequests = append(garbageRequests,
					GarbageRequest{
						Owner:     event.Installation.Account.Login,
						Repo:      repo.Name,
						Functions: []string{},
					},
				)
			}
			garbageCollect(garbageRequests)
			break
		case "deleted":
			garbageRequests := []GarbageRequest{}
			owner := event.Installation.Account.Login
			fmt.Printf("Need to remove all repos for owner: %s.\n", owner)

			garbageRequests = append(garbageRequests,
				GarbageRequest{
					Owner:     owner,
					Repo:      "*",
					Functions: []string{},
				},
			)

			garbageCollect(garbageRequests)

			break
		}

	}

	return fmt.Sprintf("Message received with event: %s", eventHeader)
}

func garbageCollect(garbageRequests []GarbageRequest) error {
	client := http.Client{}

	gatewayURL := os.Getenv("gateway_url")

	payloadSecret, err := sdk.ReadSecret("payload-secret")
	if err != nil {
		return err
	}

	for _, garbageRequest := range garbageRequests {

		body, _ := json.Marshal(garbageRequest)
		bodyReader := bytes.NewReader(body)
		req, _ := http.NewRequest(http.MethodPost, gatewayURL+"async-function/garbage-collect", bodyReader)

		digest := hmac.Sign(body, []byte(payloadSecret))
		req.Header.Add(sdk.CloudSignatureHeader, "sha1="+hex.EncodeToString(digest))

		res, err := client.Do(req)
		if err != nil {
			return err
		}
		if res.Body != nil {
			defer res.Body.Close()
		}
		if res.StatusCode != http.StatusAccepted {
			log.Printf("Unexpected status code for function: `%s` - %d\n", garbageRequest.Repo, res.StatusCode)
			resBody, _ := ioutil.ReadAll(res.Body)
			fmt.Printf("Error in garbageCollect: %s\n", resBody)
		}
	}
	return nil
}

type GarbageRequest struct {
	Functions []string `json:"functions"`
	Repo      string   `json:"repo"`
	Owner     string   `json:"owner"`
}

type InstallationRepositoriesEvent struct {
	Action       string `json:"action"`
	Installation struct {
		Account struct {
			Login string
		}
	} `json:"installation"`
	RepositoriesRemoved []Installation `json:"repositories_removed"`
	RepositoriesAdded   []Installation `json:"repositories_added"`
	Repositories        []Installation `json:"repositories"`
}

type Installation struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
}

func forward(req []byte, function string, headers map[string]string) (string, int, error) {
	payloadSecret, err := sdk.ReadSecret("payload-secret")
	if err != nil {
		return "", http.StatusInternalServerError, err
	}

	c := http.Client{}

	bodyReader := bytes.NewBuffer(req)
	pushReq, _ := http.NewRequest(http.MethodPost, os.Getenv("gateway_url")+"function/"+function, bodyReader)
	digest := hmac.Sign(req, []byte(payloadSecret))
	pushReq.Header.Add(sdk.CloudSignatureHeader, "sha1="+hex.EncodeToString(digest))

	for k, v := range headers {
		pushReq.Header.Add(k, v)
	}

	res, err := c.Do(pushReq)
	if err != nil {
		msg := "cannot post to " + function + ": " + err.Error()
		auditEvent := sdk.AuditEvent{
			Message: msg,
			Source:  Source,
		}
		sdk.PostAudit(auditEvent)
		return "", http.StatusInternalServerError, fmt.Errorf(msg)
	}

	if res.Body != nil {
		defer res.Body.Close()
	}
	body, _ := ioutil.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK {
		err = fmt.Errorf(string(body))
	}

	return string(body), res.StatusCode, err
}

func validCustomer(customers []string, owner string) bool {
	for _, customer := range customers {
		if len(customer) > 0 &&
			strings.EqualFold(customer, owner) {
			return true
		}
	}
	return false
}

// getCustomers reads a list of customers separated by new lines
// who are valid users of OpenFaaS cloud
func getCustomers(customerURL string) ([]string, error) {
	customers := []string{}

	if len(customerURL) == 0 {
		return nil, fmt.Errorf("customerURL was nil")
	}

	httpReq, reqErr := http.NewRequest(http.MethodGet, customerURL, nil)
	if reqErr != nil {
		return nil, fmt.Errorf("Error while making the request to `%s` : %s", customerURL, reqErr.Error())
	}

	c := http.Client{}
	res, reqErr := c.Do(httpReq)
	if reqErr != nil {
		return nil, fmt.Errorf("Error while requesting customers: %s", reqErr.Error())
	}

	if res.Body != nil {
		defer res.Body.Close()

		pageBody, readErr := ioutil.ReadAll(res.Body)
		if readErr != nil {
			return nil, fmt.Errorf("Error while reading response body for customers: %s", readErr)
		}

		customers = strings.Split(string(pageBody), "\n")

		for i, c := range customers {
			customers[i] = strings.ToLower(strings.TrimSuffix(c, "\r"))
		}
	}

	return customers, nil
}

func readBool(key string) bool {
	if val, exists := os.LookupEnv(key); exists {
		return val == "true" || val == "1"
	}
	return false
}
