package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/google/logger"
	"github.com/nlopes/slack"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
)

type MessageRepoSource struct {
	ProjectId  string `json:"projectId"`
	RepoName   string `json:"repoName"`
	BranchName string `json:"branchName"`
}

type MessageSource struct {
	RepoSource MessageRepoSource `json:"repoSource"`
}

type MessageTiming struct {
	StartTime string `json:"startTime"`
	EndTime   string `json:"endTime"`
}

type MessageStep struct {
	Name       string        `json:"name"`
	Args       []string      `json:"args"`
	Timing     MessageTiming `json:"timing"`
	PullTiming MessageTiming `json:"pullTiming"`
	Status     string        `json:"status"`
}

type MessageResultsImage struct {
	Name       string        `json:"name"`
	Digest     string        `json:"digest"`
	PushTiming MessageTiming `json:"pushTiming"`
}

type MessageResults struct {
	Images           []MessageResultsImage `json:"images"`
	BuildStepImages  []string              `json:"buildStepImages"`
	BuildStepOutputs []string              `json:"buildStepOutputs"`
}

type MessageArtifacts struct {
	Images []string `json:"images"`
}

type MessageResolvedRepoSource struct {
	ProjectId string `json:"projectId"`
	RepoName  string `json:"repoName"`
	CommitSha string `json:"commitSha"`
}

type MessageSourceProvenance struct {
	ResolvedRepoSource MessageResolvedRepoSource `json:"resolvedRepoSource"`
}

type MessageOptions struct {
	SubstitutionOption string `json:"substitutionOption"`
	Logging            string `json:"logging"`
}

type MessageGeneralTiming struct {
	Build       MessageTiming `json:"BUILD"`
	FetchSource MessageTiming `json:"FETCHSOURCE"`
	Push        MessageTiming `json:"PUSH"`
}

type Message struct {
	Id               string                  `json:"id"`
	ProjectId        string                  `json:"projectId"`
	Status           string                  `json:"status"`
	Source           MessageSource           `json:"source"`
	Steps            []MessageStep           `json:"steps"`
	Results          MessageResults          `json:"results"`
	CreateTime       string                  `json:"createTime"`
	StartTime        string                  `json:"startTime"`
	FinishTime       string                  `json:"finishTime"`
	Timeout          string                  `json:"timeout"`
	Images           []string                `json:"images"`
	Artifacts        MessageArtifacts        `json:"artifacts"`
	LogsBucket       string                  `json:"logsBucket"`
	SourceProvenance MessageSourceProvenance `json:"sourceProvenance"`
	BuildTriggerId   string                  `json:"buildTriggerId"`
	Options          MessageOptions          `json:"options"`
	LogUrl           string                  `json:"logUrl"`
	Tags             []string                `json:"tags"`
	Timing           MessageGeneralTiming    `json:"timing"`
}

type ResponseMessage struct {
	Success bool   `json:"error"`
	Message string `json:"message"`
}

// GLOBAL VARIABLES
var hmacSecret string
var slackWebhookUrl string
var globalLogger *logger.Logger
var kubeSet *kubernetes.Clientset

/// HMAC signature generation
func CreateSignature(input []byte, key string) string {
	signatureKey := []byte(key)

	h := hmac.New(sha1.New, signatureKey)
	h.Write(input)

	return "sha1=" + hex.EncodeToString(h.Sum(nil))
}

func Webhook(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || r.Method != "POST" {
		globalLogger.Warning(r.Method, " ", r.URL.Path, " from ", r.RemoteAddr)
		http.NotFound(w, r)
		return
	}

	globalLogger.Info(r.Method, " ", r.URL.Path, " from ", r.RemoteAddr)

	// Read body
	bytes, err := ioutil.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Check hmac signature
	signature := CreateSignature(bytes, hmacSecret)
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("x-hub-signature")), []byte(signature)) != 1 {
		globalLogger.Warning("Signature verification failed for host " + r.RemoteAddr)

		http.Error(w, "hmac signature verification failed", 401)
		return
	}

	var body Message
	if err = json.Unmarshal(bytes, &body); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if len(body.Images) < 1 || body.Images[0] == "" {
		http.Error(w, "cannot update without a new image tag", 400)
		return
	}

	// Respond as early as possible to the webhook
	message := ResponseMessage{Success: true, Message: "Sucessfully parsed " + body.Source.RepoSource.RepoName}
	output, err := json.Marshal(message)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.Write(output)

	// Deploy new version if possible
	globalLogger.Info(fmt.Sprintf("Deploying new version of %s on branch %s. Cloud Build ID: %s", body.Source.RepoSource.RepoName, body.Source.RepoSource.BranchName, body.Id))

	labelKey := "kube.volkn.cloud/cloud-build-cd-name_" + strings.ToLower(body.Source.RepoSource.RepoName)
	deployments, err := kubeSet.AppsV1().Deployments("").List(metav1.ListOptions{LabelSelector: labelKey})
	if err != nil {
		globalLogger.Error("Could not get deployments")
		globalLogger.Error(err)
		return
	}
	globalLogger.Info(fmt.Sprintf("Got %d deployments with the correct cd label", len(deployments.Items)))

	for _, deployment := range deployments.Items {
		labelValue := deployment.Labels[labelKey]

		// Convert label value to DeploymentLabelValue. Currently <branchName>.<containerPosition>
		labelValues := strings.Split(labelValue, ".")
		if len(labelValues) != 2 {
			globalLogger.Warning("Label value for deployment " + deployment.Name + " in namespace " + deployment.Namespace + " is malformed. Exactly two dot separated values are required. Skipping the deployment...")
			continue
		}
		labelBranchName := labelValues[0]
		labelContainerPosition, err := strconv.Atoi(labelValues[1])
		if err != nil {
			globalLogger.Warning("Label value for deployment " + deployment.Name + " in namespace " + deployment.Namespace + " is malformed. Second value is required to be an integer. Skipping the deployment...")
			continue
		}

		if labelBranchName != body.Source.RepoSource.BranchName {
			globalLogger.Info(fmt.Sprintf("Skipping deployment of %s in namespace %s. Branch mismatch.", deployment.Name, deployment.Namespace))
			continue
		}

		globalLogger.Info(fmt.Sprintf("Deployment %s in namespace %s is ready to be updated...", deployment.Name, deployment.Namespace))

		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Retrieve the latest version of Deployment before attempting update
			result, getErr := kubeSet.AppsV1().Deployments(deployment.Namespace).Get(deployment.Name, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}

			if len(result.Spec.Template.Spec.Containers) > labelContainerPosition {
				result.Spec.Template.Spec.Containers[labelContainerPosition].Image = body.Images[0]
				_, updateErr := kubeSet.AppsV1().Deployments(deployment.Namespace).Update(result)

				return updateErr
			}

			globalLogger.Warning(fmt.Sprintf("Label %s contains an invalid container position for deployment %s in namespace %s", labelValue, deployment.Name, deployment.Namespace))

			return errors.New("label contains invalid container position")
		})
		if retryErr != nil {
			globalLogger.Error(fmt.Sprintf("Failure updating deployment %s. Cannot retry. --- %s", deployment.Name, retryErr))
		} else {
			successText := fmt.Sprintf("Successfully updated deployment %s in namespace %s with the newest image tag.", deployment.Name, deployment.Namespace)

			globalLogger.Info(successText)

			// Slack notification
			slackMsg := slack.WebhookMessage{Text: successText}
			err := slack.PostWebhook(slackWebhookUrl, &slackMsg)
			if err != nil {
				globalLogger.Warning("Couldn't notify slack for deployment update.")
			}
		}
	}
}

func main() {
	// Setup logger
	globalLogger = logger.Init("ConsoleLogger", true, false, ioutil.Discard)

	// Get hmac secret
	hmacSecret = os.Getenv("HMAC_SECRET")
	if hmacSecret == "" || len(hmacSecret) < 32 {
		globalLogger.Fatal("HMAC_SECRET empty or too weak. Please change it accordingly.")
		panic("HMAC_SECRET too weak")
	}

	// Get Slack webhook url, setup slack api
	slackWebhookUrl = os.Getenv("SLACK_URL")
	if slackWebhookUrl == "" {
		globalLogger.Fatal("SLACK_URL not provided.")
		panic("SLACK_URL not provided")
	}

	// Setup kube cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// Set global kubeSet
	kubeSet = clientset

	var port string = os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	globalLogger.Info("Server listening on port " + port)

	http.HandleFunc("/", Webhook)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		panic(err)
	}
}
