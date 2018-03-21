package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"

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
}

// Image represents a docker image in registry
type Image struct {
	Name          string
	Tag           string
	UsedInCluster bool

	sync.RWMutex
}

type kubeConfigFlags []string

// Cfg represents the global instance configuration
var Cfg = &Config{}

const (
	registryTokenURL = "%s/jwt/auth?client_id=docker&offline_token=true&service=container_registry&scope=repository:%s:*"
	imageTagsURL     = "%s/v2/%s/tags/list"
)

func main() {
	flag.StringVar(&Cfg.GitlabURL, "giturl", "", "URL to gitlab instance")
	flag.StringVar(&Cfg.RegistryURL, "registryurl", "", "URL to gitlab docker registry")
	flag.StringVar(&Cfg.Username, "user", "", "Username used to access repository")
	flag.StringVar(&Cfg.Password, "password", "", "Password used to access repository")
	flag.StringVar(&Cfg.Repository, "repository", "", "Lookup this specific repository. Include group if repo is in a group.")
	flag.Var(&Cfg.KubeConfig, "kubeconfig", "absolute path to the kubeconfig file")
	flag.Parse()

	// Parse registry url (remove protocol)
	Cfg.RegistryURLShort = strings.Replace(Cfg.RegistryURL, "https://", "", 1)
	Cfg.RegistryURLShort = strings.Replace(Cfg.RegistryURLShort, "http://", "", 1)

	// --- Get gitlab registry token ---
	token := getRegistryToken()

	// --- Get all image tags from the repository ---
	images := getImages(token)

	// --- Print out found tags ---
	for _, image := range images {
		fmt.Printf("Tag found: %s:%s\n", image.Name, image.Tag)
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
			fmt.Printf("Image not used in cluster: %s:%s\n", image.Name, image.Tag)
		}
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

func getImages(token string) []*Image {
	// Create request
	listTagsURL := fmt.Sprintf(imageTagsURL, Cfg.RegistryURL, Cfg.Repository)
	req, err := http.NewRequest("GET", listTagsURL, nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
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
		panic("error")
	}

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

func (k *kubeConfigFlags) Set(value string) error {
	*k = append(*k, value)
	return nil
}

func (k *kubeConfigFlags) String() string {
	return ""
}
