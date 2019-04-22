package main

import (
	"encoding/json"
	"fmt"
	"github.com/google/logger"
	"io/ioutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"net/http"
	"os"
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
var globalLogger *logger.Logger
var kubeSet *kubernetes.Clientset

func Webhook(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" || r.Method != "POST" {
		globalLogger.Warning(r.Method, " ", r.URL.Path, " from ", r.Host)
		http.NotFound(w, r)
		return
	}

	globalLogger.Info(r.Method, " ", r.URL.Path, " from ", r.Host)

	// Read body
	bytes, err := ioutil.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	var body Message
	if err = json.Unmarshal(bytes, &body); err != nil {
		http.Error(w, err.Error(), 500)
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

	deployments, err := kubeSet.ExtensionsV1beta1().Deployments("").List(metav1.ListOptions{LabelSelector: "kube.volkn.cloud/cloud-build-cd-name=" + body.Source.RepoSource.RepoName})
	if err != nil {
		panic(err.Error())
	}
	globalLogger.Info(fmt.Sprintf("Got %d deployments with the correct cd label", len(deployments.Items)))

	for _, deployment := range deployments.Items {
		if deployment.Labels["kube.volkn.cloud/cloud-build-cd-name"] == body.Source.RepoSource.RepoName {
			globalLogger.Info(fmt.Sprintf("Deployment %s in namespace %s is ready to be updated...", deployment.Name, deployment.Namespace))
		}
	}

	deployments2, err := kubeSet.AppsV1().Deployments("").List(metav1.ListOptions{LabelSelector: "kube.volkn.cloud/cloud-build-cd-name=" + body.Source.RepoSource.RepoName})
	if err != nil {
		panic(err.Error())
	}
	globalLogger.Info(fmt.Sprintf("Got %d deployments with the correct cd label", len(deployments2.Items)))
}

func main() {
	// Setup logger
	globalLogger = logger.Init("ConsoleLogger", true, false, ioutil.Discard)

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
	if err := http.ListenAndServe(":" + port, nil); err != nil {
		panic(err)
	}
}
