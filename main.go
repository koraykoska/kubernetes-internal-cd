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

type MessageGithub struct {
	Sha        string `json:"sha"`
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
}

type Message struct {
	Github MessageGithub `json:"github"`
	Image  string        `json:"image"`
}

type ResponseMessage struct {
	Success bool   `json:"error"`
	Message string `json:"message"`
}

// GLOBAL VARIABLES
var slackWebhookUrl string
var globalLogger *logger.Logger
var kubeSet *kubernetes.Clientset

/// HMAC signature generation
func CreateSignature(input []byte, key []byte) []byte {
	// signatureKey := []byte(key)

	h := hmac.New(sha1.New, key)
	h.Write(input)

	return h.Sum(nil)
}

/// Create a signature hash "sha1=..." from the given signature
func CreateSignatureHash(signature []byte) string {
	return "sha1=" + hex.EncodeToString(signature)
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

	// Decode body
	var body Message
	if err = json.Unmarshal(bytes, &body); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Get hmac master key
	secret, err := kubeSet.CoreV1().Secrets(os.Getenv("SECRET_NAMESPACE")).Get(os.Getenv("SECRET_NAME"), metav1.GetOptions{})
	if err != nil {
		globalLogger.Error("Could not get secret")
		globalLogger.Error(err)
		return
	}
	hmacSecret := CreateSignature([]byte(body.Github.Repository), secret.Data["master_key"])
	hmacSecretOld := CreateSignature([]byte(body.Github.Repository), secret.Data["master_key_old"])

	// Check hmac signature
	signature := CreateSignatureHash(CreateSignature(bytes, hmacSecret))
	signatureOld := CreateSignatureHash(CreateSignature(bytes, hmacSecretOld))
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("x-hub-signature")), []byte(signature)) != 1 &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get("x-hub-signature")), []byte(signatureOld)) != 1 {
		globalLogger.Warning("Signature verification failed for host " + r.RemoteAddr)

		http.Error(w, "hmac signature verification failed", 401)
		return
	}

	// Respond as early as possible to the webhook
	message := ResponseMessage{Success: true, Message: "Sucessfully parsed " + body.Github.Repository}
	output, err := json.Marshal(message)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.Write(output)

	// Deploy new version if possible
	globalLogger.Info(fmt.Sprintf("Deploying new version of %s on branch %s", body.Github.Repository, body.Github.Ref))

	labelKey := "ki-cd/" + strings.Replace(strings.ToLower(body.Github.Repository), "/", "_", -1)

	deployments, err := kubeSet.AppsV1().Deployments("").List(metav1.ListOptions{LabelSelector: labelKey})
	if err != nil {
		globalLogger.Error("Could not get deployments")
		globalLogger.Error(err)
		return
	}
	globalLogger.Info(fmt.Sprintf("Got %d deployments with the correct cd label", len(deployments.Items)))

	statefulSets, err := kubeSet.AppsV1().StatefulSets("").List(metav1.ListOptions{LabelSelector: labelKey})
	if err != nil {
		globalLogger.Error("Could not get stateful sets")
		globalLogger.Error(err)
		return
	}
	globalLogger.Info(fmt.Sprintf("Got %d stateful sets with the correct cd label", len(statefulSets.Items)))

	// Update deployments
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

		if labelBranchName != strings.TrimPrefix(body.Github.Ref, "refs/heads/") {
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
				result.Spec.Template.Spec.Containers[labelContainerPosition].Image = fmt.Sprintf("%s:%s", body.Image, body.Github.Sha)
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

	// Same for stateful sets...
	for _, statefulSet := range statefulSets.Items {
		labelValue := statefulSet.Labels[labelKey]

		// Convert label value to DeploymentLabelValue. Currently <branchName>.<containerPosition>
		labelValues := strings.Split(labelValue, ".")
		if len(labelValues) != 2 {
			globalLogger.Warning("Label value for statefulSet " + statefulSet.Name + " in namespace " + statefulSet.Namespace + " is malformed. Exactly two dot separated values are required. Skipping the deployment...")
			continue
		}
		labelBranchName := labelValues[0]
		labelContainerPosition, err := strconv.Atoi(labelValues[1])
		if err != nil {
			globalLogger.Warning("Label value for statefulSet " + statefulSet.Name + " in namespace " + statefulSet.Namespace + " is malformed. Second value is required to be an integer. Skipping the deployment...")
			continue
		}

		if labelBranchName != strings.TrimPrefix(body.Github.Ref, "refs/heads/") {
			globalLogger.Info(fmt.Sprintf("Skipping statefulSet of %s in namespace %s. Branch mismatch.", statefulSet.Name, statefulSet.Namespace))
			continue
		}

		globalLogger.Info(fmt.Sprintf("StatefulSet %s in namespace %s is ready to be updated...", statefulSet.Name, statefulSet.Namespace))

		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Retrieve the latest version of StatefulSet before attempting update
			result, getErr := kubeSet.AppsV1().StatefulSets(statefulSet.Namespace).Get(statefulSet.Name, metav1.GetOptions{})
			if getErr != nil {
				return getErr
			}

			if len(result.Spec.Template.Spec.Containers) > labelContainerPosition {
				result.Spec.Template.Spec.Containers[labelContainerPosition].Image = fmt.Sprintf("%s:%s", body.Image, body.Github.Sha)
				_, updateErr := kubeSet.AppsV1().StatefulSets(statefulSet.Namespace).Update(result)

				return updateErr
			}

			globalLogger.Warning(fmt.Sprintf("Label %s contains an invalid container position for statefulSet %s in namespace %s", labelValue, statefulSet.Name, statefulSet.Namespace))

			return errors.New("label contains invalid container position")
		})
		if retryErr != nil {
			globalLogger.Error(fmt.Sprintf("Failure updating statefulSet %s. Cannot retry. --- %s", statefulSet.Name, retryErr))
		} else {
			successText := fmt.Sprintf("Successfully updated statefulSet %s in namespace %s with the newest image tag.", statefulSet.Name, statefulSet.Namespace)

			globalLogger.Info(successText)

			// Slack notification
			slackMsg := slack.WebhookMessage{Text: successText}
			err := slack.PostWebhook(slackWebhookUrl, &slackMsg)
			if err != nil {
				globalLogger.Warning("Couldn't notify slack for statefulSet update.")
			}
		}
	}
}

func main() {
	// Setup logger
	globalLogger = logger.Init("ConsoleLogger", true, false, ioutil.Discard)

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
