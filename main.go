package main

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kingpin"
	manifest "github.com/estafette/estafette-ci-manifest"
)

var (
	version   string
	branch    string
	revision  string
	buildDate string
	goVersion = runtime.Version()
)

var (
	credentialsJSON = kingpin.Flag("credentials", "Container registry credentials configured at the CI server, passed in to this trusted extension.").Envar("ESTAFETTE_CREDENTIALS_CONTAINER_REGISTRY").String()
	stagesJSON      = kingpin.Flag("stages", "Executed stages, to determine what images to prefetch.").Envar("ESTAFETTE_STAGES").String()
)

func main() {

	// parse command line parameters
	kingpin.Parse()

	// log to stdout and hide timestamp
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Flags() &^ (log.Ldate | log.Ltime))

	// log startup message
	log.Printf("Starting estafette-extension-prefetch version %v...", version)

	// get api token from injected credentials
	var credentials []ContainerRegistryCredentials
	if *credentialsJSON != "" {
		err := json.Unmarshal([]byte(*credentialsJSON), &credentials)
		if err != nil {
			log.Fatal("Failed unmarshalling injected credentials: ", err)
		}
	}

	// unmarshal stages
	var stages []*manifest.EstafetteStage
	if *stagesJSON != "" {
		err := json.Unmarshal([]byte(*stagesJSON), &stages)
		if err != nil {
			log.Fatal("Failed unmarshalling injected stages: ", err)
		}
	}

	prefetchStart := time.Now()

	// deduplicate stages by image path
	dedupedStages := []*manifest.EstafetteStage{}
	for _, p := range stages {

		// test if it's already added
		alreadyAdded := false
		for _, d := range dedupedStages {
			if p.ContainerImage == d.ContainerImage {
				alreadyAdded = true
				break
			}
		}

		// added if it hasn't been added before
		if !alreadyAdded {
			dedupedStages = append(dedupedStages, p)
		}
	}

	var wg sync.WaitGroup
	wg.Add(len(dedupedStages))

	// pull all images in parallel
	for _, p := range dedupedStages {
		go func(p manifest.EstafetteStage) {
			defer wg.Done()
			loginIfRequired(credentials, p.ContainerImage)
			log.Printf("Pulling container image %v\n", p.ContainerImage)
			pullArgs := []string{
				"pull",
				p.ContainerImage,
			}
			runCommand("docker", pullArgs)
		}(*p)
	}

	// wait for all pulls to finish
	wg.Wait()
	prefetchDuration := time.Since(prefetchStart)

	log.Printf("Done prefetching %v images in %v seconds", len(dedupedStages), prefetchDuration.Seconds())
}

func getCredentialsForContainers(credentials []ContainerRegistryCredentials, containerImages []string) map[string]*ContainerRegistryCredentials {

	filteredCredentialsMap := make(map[string]*ContainerRegistryCredentials, 0)

	if credentials != nil {
		// loop all container images
		for _, ci := range containerImages {
			containerImageSlice := strings.Split(ci, "/")
			containerRepo := strings.Join(containerImageSlice[:len(containerImageSlice)-1], "/")

			if _, ok := filteredCredentialsMap[containerRepo]; ok {
				// credentials for this repo were added before, check next container image
				continue
			}

			// find the credentials matching the container image
			for _, credential := range credentials {
				if containerRepo == credential.AdditionalProperties.Repository {
					// this one matches, add it to the map
					filteredCredentialsMap[credential.AdditionalProperties.Repository] = &credential
					break
				}
			}
		}
	}

	return filteredCredentialsMap
}

var (
	imagesFromDockerFileRegex *regexp.Regexp
)

func getFromImagePathsFromDockerfile(dockerfileContent []byte) ([]string, error) {

	containerImages := []string{}

	if imagesFromDockerFileRegex == nil {
		imagesFromDockerFileRegex = regexp.MustCompile(`(?im)^FROM\s*([^\s]+)(\s*AS\s[a-zA-Z0-9]+)?\s*$`)
	}

	matches := imagesFromDockerFileRegex.FindAllStringSubmatch(string(dockerfileContent), -1)

	if len(matches) > 0 {
		for _, m := range matches {
			if len(m) > 1 {
				// check if it's not an official docker hub image
				if strings.Count(m[1], "/") != 0 {
					containerImages = append(containerImages, m[1])
				}
			}
		}
	}

	return containerImages, nil
}

func loginIfRequired(credentials []ContainerRegistryCredentials, containerImages ...string) {

	log.Printf("Filtering credentials for images %v\n", containerImages)

	// retrieve all credentials
	filteredCredentialsMap := getCredentialsForContainers(credentials, containerImages)

	log.Printf("Filtered %v container-registry credentials down to %v\n", len(credentials), len(filteredCredentialsMap))

	if filteredCredentialsMap != nil {
		for _, c := range filteredCredentialsMap {
			if c != nil {
				log.Printf("Logging in to repository '%v'\n", c.AdditionalProperties.Repository)
				loginArgs := []string{
					"login",
					"--username",
					c.AdditionalProperties.Username,
					"--password",
					c.AdditionalProperties.Password,
				}

				repositorySlice := strings.Split(c.AdditionalProperties.Repository, "/")
				if len(repositorySlice) > 1 {
					server := repositorySlice[0]
					loginArgs = append(loginArgs, server)
				}

				err := exec.Command("docker", loginArgs...).Run()
				handleError(err)
			}
		}
	}
}

func handleError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func runCommand(command string, args []string) {
	err := runCommandExtended(command, args)
	handleError(err)
}

func runCommandExtended(command string, args []string) error {
	log.Printf("Running command '%v %v'...", command, strings.Join(args, " "))
	cmd := exec.Command(command, args...)
	cmd.Dir = "/estafette-work"
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return err
}