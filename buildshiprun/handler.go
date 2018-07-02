package function

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openfaas/openfaas-cloud/sdk"

	"github.com/alexellis/derek/auth"
	"github.com/google/go-github/github"
)

const (
	defaultPrivateKeyName = "private-key"
)

var (
	imageValidator = regexp.MustCompile("(?:[a-zA-Z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?/[a-zA-Z0-9]+(?:[._-][a-z0-9]+)*/)*[a-zA-Z0-9]+(?:[._-][a-z0-9]+)*")
)

// Handle a build / deploy request - returns empty string for an error
func Handle(req []byte) string {

	c := &http.Client{}

	builderURL := os.Getenv("builder_url")

	event, eventErr := getEvent()
	if eventErr != nil {
		log.Panic(eventErr)
	}

	auditEvent := sdk.AuditEvent{
		Owner:  event.owner,
		Repo:   event.repository,
		Source: "buildshiprun",
	}

	reader := bytes.NewBuffer(req)

	r, _ := http.NewRequest(http.MethodPost, builderURL+"build", reader)
	r.Header.Set("Content-Type", "application/octet-stream")

	res, err := c.Do(r)

	if err != nil {
		fmt.Println(err)
		reportStatus("failure", err.Error(), "BUILD", event)
		auditEvent.Message = fmt.Sprintf("buildshiprun failure: %s", err.Error())
		sdk.PostAudit(auditEvent)
		return auditEvent.Message
	}

	defer res.Body.Close()

	buildStatus, _ := ioutil.ReadAll(res.Body)
	imageName := strings.TrimSpace(string(buildStatus))

	repositoryURL := os.Getenv("repository_url")
	pushRepositoryURL := os.Getenv("push_repository_url")

	if len(repositoryURL) == 0 {
		fmt.Fprintf(os.Stderr, "repository_url env-var not set")
		os.Exit(1)
	}

	if len(pushRepositoryURL) == 0 {
		fmt.Fprintf(os.Stderr, "push_repository_url env-var not set")
		os.Exit(1)
	}

	serviceValue := ""

	log.Printf("buildshiprun: image '%s'\n", imageName)

	if !validImage(imageName) {
		msg := "Unable to build image, check builder logs"
		reportStatus("failure", msg, "DEPLOY", event)
		log.Fatal(msg)
		auditEvent.Message = fmt.Sprintf("buildshiprun failure: %s", msg)
		sdk.PostAudit(auditEvent)
		return msg
	}

	if len(imageName) > 0 {
		gatewayURL := os.Getenv("gateway_url")

		// Replace image name for "localhost" for deployment
		imageName = getImageName(repositoryURL, pushRepositoryURL, imageName)

		serviceValue = fmt.Sprintf("%s-%s", event.owner, event.service)

		log.Printf("Deploying %s as %s", imageName, serviceValue)

		defaultMemoryLimit := os.Getenv("default_memory_limit")
		if len(defaultMemoryLimit) == 0 {
			defaultMemoryLimit = "20m"
		}

		deploy := deployment{
			Service: serviceValue,
			Image:   imageName,
			Network: "func_functions",
			Labels: map[string]string{
				"Git-Cloud":      "1",
				"Git-Owner":      event.owner,
				"Git-Repo":       event.repository,
				"Git-DeployTime": strconv.FormatInt(time.Now().Unix(), 10), //Unix Epoch string
				"Git-SHA":        event.sha,
				"faas_function":  serviceValue,
				"app":            serviceValue,
			},
			Limits: Limits{
				Memory: defaultMemoryLimit,
			},
			EnvVars: event.environment,
			Secrets: event.secrets,
		}

		result, err := deployFunction(deploy, gatewayURL, c)

		if err != nil {
			reportStatus("failure", err.Error(), "DEPLOY", event)
			log.Fatal(err.Error())
			auditEvent.Message = fmt.Sprintf("buildshiprun failure: %s", err.Error())
		} else {
			auditEvent.Message = fmt.Sprintf("buildshiprun succeeded: deployed %s", imageName)
		}

		log.Println(result)
	}

	sdk.PostAudit(auditEvent)

	reportStatus("success", fmt.Sprintf("function successfully deployed as: %s", serviceValue), "DEPLOY", event)
	return fmt.Sprintf("buildStatus %s %s %s", buildStatus, imageName, res.Status)
}

func getEvent() (*eventInfo, error) {
	var err error
	info := eventInfo{}

	info.service = os.Getenv("Http_Service")
	info.owner = os.Getenv("Http_Owner")
	info.repository = os.Getenv("Http_Repo")
	info.sha = os.Getenv("Http_Sha")
	info.url = os.Getenv("Http_Url")
	info.image = os.Getenv("Http_Image")

	if len(os.Getenv("Http_Installation_id")) > 0 {
		info.installationID, err = strconv.Atoi(os.Getenv("Http_Installation_id"))
	}

	httpEnv := os.Getenv("Http_Env")
	envVars := make(map[string]string)

	if len(httpEnv) > 0 {
		envErr := json.Unmarshal([]byte(httpEnv), &envVars)

		if envErr == nil {
			info.environment = envVars
		} else {
			log.Printf("Error un-marshaling env-vars for function %s, %s", info.service, envErr)
			info.environment = make(map[string]string)
		}
	}

	secretVars := []string{}
	secretsStr := os.Getenv("Http_Secrets")

	if len(secretsStr) > 0 {
		secretErr := json.Unmarshal([]byte(secretsStr), &secretVars)

		if secretErr != nil {
			log.Println(secretErr)
		}

	}

	info.secrets = secretVars

	for i := 0; i < len(info.secrets); i++ {
		info.secrets[i] = info.owner + "-" + info.secrets[i]
	}

	log.Printf("%d env-vars for %s", len(info.environment), info.service)

	return &info, err
}

func functionExists(deploy deployment, gatewayURL string, c *http.Client) (bool, error) {

	r, _ := http.NewRequest(http.MethodGet, gatewayURL+"system/functions", nil)

	addAuthErr := sdk.AddBasicAuth(r)
	if addAuthErr != nil {
		log.Printf("Basic auth error %s", addAuthErr)
	}

	res, err := c.Do(r)

	if err != nil {
		fmt.Println(err)
		return false, err
	}

	defer res.Body.Close()

	fmt.Println("functionExists status: " + res.Status)
	result, _ := ioutil.ReadAll(res.Body)

	functions := []function{}
	json.Unmarshal(result, &functions)

	for _, function1 := range functions {
		if function1.Name == deploy.Service {
			return true, nil
		}
	}

	return false, err
}

func deployFunction(deploy deployment, gatewayURL string, c *http.Client) (string, error) {
	exists, err := functionExists(deploy, gatewayURL, c)

	bytesOut, _ := json.Marshal(deploy)

	reader := bytes.NewBuffer(bytesOut)

	fmt.Println("Deploying: " + deploy.Image + " as " + deploy.Service)
	var res *http.Response
	var httpReq *http.Request
	var method string
	if exists {
		method = http.MethodPut
	} else {
		method = http.MethodPost
	}

	httpReq, err = http.NewRequest(method, gatewayURL+"system/functions", reader)
	httpReq.Header.Set("Content-Type", "application/json")

	addAuthErr := sdk.AddBasicAuth(httpReq)
	if addAuthErr != nil {
		log.Printf("Basic auth error %s", addAuthErr)
	}

	res, err = c.Do(httpReq)

	if err != nil {
		fmt.Println(err)
		return "", err
	}

	defer res.Body.Close()

	fmt.Println("Deploy status: " + res.Status)
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", fmt.Errorf("http status code %d", res.StatusCode)
	}
	buildStatus, _ := ioutil.ReadAll(res.Body)

	return string(buildStatus), err
}

func enableStatusReporting() bool {
	return os.Getenv("report_status") == "true"
}

func buildPublicStatusURL(status string, event *eventInfo) string {
	url := event.url

	if status == "success" {
		publicURL := os.Getenv("gateway_public_url")
		gatewayPrettyURL := os.Getenv("gateway_pretty_url")

		if len(gatewayPrettyURL) > 0 {
			// gateway_pretty_url=
			// https://user.get-faas.com/function
			url = strings.Replace(gatewayPrettyURL, "user", event.owner, 1)
			url = strings.Replace(url, "function", event.service, 1)
		} else if len(publicURL) > 0 {
			if strings.HasSuffix(publicURL, "/") == false {
				publicURL = publicURL + "/"
			}
			// for success status if gateway's public url id set the deployed
			// function url is used in the commit status
			serviceValue := fmt.Sprintf("%s-%s", event.owner, event.service)
			url = publicURL + "function/" + serviceValue
		}
	}

	return url
}

func reportStatus(status string, desc string, statusContext string, event *eventInfo) {

	if !enableStatusReporting() {
		return
	}

	url := buildPublicStatusURL(status, event)
	ctx := context.Background()
	privateKeyPath := getPrivateKey()

	repoStatus := buildStatus(status, desc, statusContext, url)

	log.Printf("Status: %s, GitHub AppID: %d, Repo: %s, Owner: %s", status, event.installationID, event.repository, event.owner)

	token, tokenErr := auth.MakeAccessTokenForInstallation(os.Getenv("github_app_id"), event.installationID, privateKeyPath)

	if tokenErr != nil {
		fmt.Printf("failed to report status %v, error: %s\n", repoStatus, tokenErr.Error())
		return
	}

	if token == "" {
		fmt.Printf("failed to report status %v, error: authentication failed Invalid token\n", repoStatus)
		return
	}

	client := auth.MakeClient(ctx, token)

	_, _, apiErr := client.Repositories.CreateStatus(ctx, event.owner, event.repository, event.sha, repoStatus)
	if apiErr != nil {
		fmt.Printf("failed to report status %v, error: %s\n", repoStatus, apiErr.Error())
		return
	}
}

func getPrivateKey() string {
	// we are taking the secrets name from the env, by default it is fixed
	// to private_key.pem.
	// Although user can make the secret with a specific name and provide
	// it in the stack.yaml and also specify the secret name in github.yml
	privateKeyName := os.Getenv("private_key")
	if privateKeyName == "" {
		privateKeyName = defaultPrivateKeyName
	}
	privateKeyPath := "/var/openfaas/secrets/" + privateKeyName
	return privateKeyPath
}

func buildStatus(status string, desc string, context string, url string) *github.RepoStatus {
	return &github.RepoStatus{State: &status, TargetURL: &url, Description: &desc, Context: &context}
}

func getImageName(repositoryURL, pushRepositoryURL, imageName string) string {

	return strings.Replace(imageName, pushRepositoryURL, repositoryURL, 1)

	// return repositoryURL + imageName[strings.Index(imageName, "/"):]
}

func validImage(image string) bool {
	match := imageValidator.FindString(image)
	// image should be the whole string
	if len(match) == len(image) {
		return true
	}
	return false
}

type eventInfo struct {
	service        string
	owner          string
	repository     string
	image          string
	sha            string
	url            string
	installationID int
	environment    map[string]string
	secrets        []string
}

type deployment struct {
	Service string
	Image   string
	Network string
	Labels  map[string]string
	Limits  Limits
	// EnvVars provides overrides for functions.
	EnvVars map[string]string `json:"envVars"`
	Secrets []string
}

type Limits struct {
	Memory string
}

type function struct {
	Name string
}
