package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// Config represents the configuration
type Config struct {
	GitlabURL        string
	RegistryURL      string
	RegistryURLShort string
	Username         string
	Password         string
	Repository       string
	KubeConfig       kubeConfigFlags
	MinExpiry        int
	RegexPattern     string
	DeleteImages     bool
}

// Image represents a docker image in registry
type Image struct {
	Name          string
	Tag           string
	Digest        string
	Created       time.Time
	UsedInCluster bool

	sync.RWMutex
}

type kubeConfigFlags []string

// Cfg represents the global instance configuration
var Cfg = &Config{}

const (
	registryTokenURL = "%s/jwt/auth?client_id=docker&offline_token=true&service=container_registry&scope=repository:%s:*"
	imageTagsURL     = "%s/v2/%s/tags/list"
	manifestURL      = "%s/v2/%s/manifests/%s"
)

func main() {
	flag.StringVar(&Cfg.GitlabURL, "giturl", "", "URL to gitlab instance")
	flag.StringVar(&Cfg.RegistryURL, "registryurl", "", "URL to gitlab docker registry")
	flag.StringVar(&Cfg.Username, "user", "", "Username used to access repository")
	flag.StringVar(&Cfg.Password, "password", "", "Password used to access repository")
	flag.StringVar(&Cfg.Repository, "repository", "", "Lookup this specific repository. Include group if repo is in a group.")
	flag.Var(&Cfg.KubeConfig, "kubeconfig", "absolute path to the kubeconfig file")
	flag.IntVar(&Cfg.MinExpiry, "minexpiry", 7, "Minimum age for images in days which shall be removed")
	flag.StringVar(&Cfg.RegexPattern, "regexp", "", "Regex pattern which must NOT match with the image tag")
	flag.BoolVar(&Cfg.DeleteImages, "delete", false, "If true, will delete all found images")
	flag.Parse()

	// Parse registry url (remove protocol)
	Cfg.RegistryURLShort = strings.Replace(Cfg.RegistryURL, "https://", "", 1)
	Cfg.RegistryURLShort = strings.Replace(Cfg.RegistryURLShort, "http://", "", 1)

	// --- Get gitlab registry token ---
	token := getRegistryToken()

	// --- Get all image tags from the repository ---
	images := getImages(token)

	// --- Set the time when the image was created ---
	setImageUploadDate(token, images)

	// --- Remove images from the slice which are too young ---
	// Calculate min expiry date
	minExpiryDate := time.Now().AddDate(0, 0, Cfg.MinExpiry*-1)

	// Remove images
	i := 0
	for _, image := range images {
		if image.Created.Before(minExpiryDate) {
			// This image should stay in slice
			images[i] = image
			i++
		} else {
			// Print information
			fmt.Printf("Image %s:%s is too young, skipped: %s\n", image.Name, image.Tag, image.Created.String())
		}
	}
	// Remove the rest
	images = images[:i]

	// --- Remove images which does not match the regex pattern if provided ---
	if Cfg.RegexPattern != "" {
		i = 0
		for _, image := range images {
			if matched, _ := regexp.MatchString(Cfg.RegexPattern, image.Tag); !matched {
				// This image should stay in slice
				images[i] = image
				i++
			} else {
				// Print information
				fmt.Printf("Image %s:%s matches regexp, skipped: %s\n", image.Name, image.Tag, Cfg.RegexPattern)
			}
		}
		// Remove the rest
		images = images[:i]
	}

	// --- Look up images in kubernetes clusters ---
	// Create wait group
	var wg sync.WaitGroup
	wg.Add(len(Cfg.KubeConfig)) // Per cluster one goroutine

	// Create goroutine per cluster
	for _, config := range Cfg.KubeConfig {
		go setImagesClusterUsage(images, config, &wg)
	}
	wg.Wait()

	// --- Print resulting images ---
	for _, image := range images {
		if !image.UsedInCluster {
			fmt.Printf("Image will be deleted: %s:%s\n", image.Name, image.Tag)
		}
	}

	// --- Give the user the chance to think about it ---
	if Cfg.DeleteImages {
		reader := bufio.NewReader(os.Stdin)
		fmt.Println("Do you really want to delete the images listed above? Please type yes if so...")
		fmt.Printf("> ")
		text, _ := reader.ReadString('\n')
		if text != "yes\n" {
			return
		}

		// Start delete process
		fmt.Println("--- Starting delete process ---")

		// Set image digest
		setImageDigest(token, images)

		// Delete images
		deleteImages(images, token)
	}

}

func setImagesClusterUsage(images []*Image, c string, wg *sync.WaitGroup) {
	defer wg.Done()
	config, err := clientcmd.BuildConfigFromFlags("", c)
	if err != nil {
		panic(err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	// get namespaces
	ns := clientset.CoreV1Client.Namespaces()
	nsList, err := ns.List(v1.ListOptions{})
	if err != nil {
		panic(err)
	}

	// iterate over all namespaces
	for _, nsObj := range nsList.Items {
		// Get all pods
		podsInterface := clientset.CoreV1Client.Pods(nsObj.Name)
		pods, err := podsInterface.List(v1.ListOptions{})
		if err != nil {
			panic(err)
		}

		// Iterate all image tags
		for id, image := range images {
			// Format full image name
			imageName := fmt.Sprintf("%s/%s:%s", Cfg.RegistryURLShort, image.Name, image.Tag)

			// Iterate all pods
			for _, pod := range pods.Items {
				// Iterate containers
				for _, cont := range pod.Spec.Containers {
					// Image the same currently in use by container?
					if imageName == cont.Image {
						images[id].Lock()
						images[id].UsedInCluster = true
						images[id].Unlock()

						// Print output
						fmt.Printf("Image %s:%s is used in Namespace %s and pod %s\n",
							image.Name, image.Tag, nsObj.Name, pod.Name)
					}
				}

				// Extra check to leave long loop
				if image.UsedInCluster {
					break
				}
			}
		}
	}
}

func deleteImages(images []*Image, token string) {
	for _, image := range images {
		// Create request
		manifestURLParsed := fmt.Sprintf(manifestURL, Cfg.RegistryURL, Cfg.Repository, image.Digest)
		sendHTTPRequest(manifestURLParsed, token, "DELETE", true)
		fmt.Printf("Image deleted: %s:%s\n", image.Name, image.Tag)
	}
}

func setImageDigest(token string, images []*Image) {
	for id, image := range images {
		// Create request
		manifestURLParsed := fmt.Sprintf(manifestURL, Cfg.RegistryURL, Cfg.Repository, image.Tag)
		body, resp := sendHTTPRequest(manifestURLParsed, token, "GET", true)

		// Extract image tags from response
		var data map[string]interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			panic(err)
		}

		// Get digest
		images[id].Digest = resp.Header.Get("Docker-Content-Digest")
	}
}

func setImageUploadDate(token string, images []*Image) {
	for id, image := range images {
		// Create request
		manifestURLParsed := fmt.Sprintf(manifestURL, Cfg.RegistryURL, Cfg.Repository, image.Tag)
		body, _ := sendHTTPRequest(manifestURLParsed, token, "GET", false)

		// Extract image tags from response
		var data map[string]interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			panic(err)
		}

		// Get history
		history := data["history"].([]interface{})

		// Get first history entry (always the newest) which is the last layer
		lastLayer := history[0].(map[string]interface{})

		// Get v1 Compatibility
		compJSON := lastLayer["v1Compatibility"].(string)

		// Get created field value
		var comp map[string]interface{}
		if err := json.Unmarshal([]byte(compJSON), &comp); err != nil {
			panic(err)
		}

		// Parse time
		t, err := time.Parse(time.RFC3339, comp["created"].(string))
		if err != nil {
			panic(err)
		}

		// Save time
		images[id].Created = t
	}
}

func getImages(token string) []*Image {
	// Create request
	listTagsURL := fmt.Sprintf(imageTagsURL, Cfg.RegistryURL, Cfg.Repository)
	body, _ := sendHTTPRequest(listTagsURL, token, "GET", false)

	// Extract image tags from response
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		panic(err)
	}

	// Get tags and image
	var images []*Image
	imageTags := data["tags"].([]interface{})
	for _, tag := range imageTags {
		images = append(images, &Image{
			Name: Cfg.Repository,
			Tag:  tag.(string),
		})
	}
	return images
}

func getRegistryToken() string {
	// Create request
	tokenURL := fmt.Sprintf(registryTokenURL, Cfg.GitlabURL, Cfg.Repository)
	req, err := http.NewRequest("GET", tokenURL, nil)
	if err != nil {
		panic(err)
	}
	req.SetBasicAuth(Cfg.Username, Cfg.Password)
	cli := &http.Client{}

	// Send request
	resp, err := cli.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	// Get response from body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	// Validate response
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Return code: %d\n", resp.StatusCode)
		fmt.Printf("Message: %s", string(body[:]))
		panic("wrong username/password or repository combination")
	}

	// Extract token from response
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		panic(err)
	}
	return data["token"].(string)
}

func sendHTTPRequest(url, token, method string, h bool) ([]byte, *http.Response) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	if h {
		req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	}
	cli := &http.Client{}

	// Send request
	resp, err := cli.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	// Get response from body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	// Validate response
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		if resp.StatusCode == http.StatusNotFound {
			fmt.Printf("Return code: %d\n", resp.StatusCode)
		} else {
			fmt.Printf("Return code: %d\n", resp.StatusCode)
			fmt.Printf("Message: %s", string(body[:]))
			panic("error")
		}
	}

	return body, resp
}

func (k *kubeConfigFlags) Set(value string) error {
	*k = append(*k, value)
	return nil
}

func (k *kubeConfigFlags) String() string {
	return ""
}
