package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Knetic/govaluate"
	"github.com/alecthomas/kingpin"
	manifest "github.com/estafette/estafette-ci-manifest"
	foundation "github.com/estafette/estafette-foundation"
	"github.com/rs/zerolog/log"
)

var (
	appgroup  string
	app       string
	version   string
	branch    string
	revision  string
	buildDate string
	goVersion = runtime.Version()
)

var (
	credentialsJSON = kingpin.Flag("credentials", "Container registry credentials configured at the CI server, passed in to this trusted extension.").Envar("ESTAFETTE_CREDENTIALS_CONTAINER_REGISTRY").Required().String()
	stagesJSON      = kingpin.Flag("stages", "Executed stages, to determine what images to prefetch.").Envar("ESTAFETTE_STAGES").Required().String()
)

func main() {

	// parse command line parameters
	kingpin.Parse()

	// init log format from envvar ESTAFETTE_LOG_FORMAT
	foundation.InitLoggingFromEnv(appgroup, app, version, branch, revision, buildDate)

	// create context to cancel commands on sigterm
	ctx := foundation.InitCancellationContext(context.Background())

	// log startup message
	log.Info().Msgf("Starting estafette-extension-prefetch version %v...", version)

	// get api token from injected credentials
	var credentials []ContainerRegistryCredentials
	if *credentialsJSON != "" {
		err := json.Unmarshal([]byte(*credentialsJSON), &credentials)
		if err != nil {
			log.Info().Msgf("Failed unmarshalling injected credentials: %v", err)
		}
	}

	// unmarshal stages
	var stages []*manifest.EstafetteStage
	if *stagesJSON != "" {
		err := json.Unmarshal([]byte(*stagesJSON), &stages)
		if err != nil {
			log.Info().Msgf("Failed unmarshalling injected stages: %v", err)
		}
	}

	if len(stages) == 0 {
		log.Info().Msg("No stages in present in environment variable ESTAFETTE_STAGES")
	}

	prefetchStart := time.Now()

	// deduplicate stages by image path
	dedupedStages := []*manifest.EstafetteStage{}
	for _, p := range stages {

		// test if the when clause passes
		whenEvaluationResult, err := evaluateWhen(p.Name, p.When, getParameters())
		if err != nil || !whenEvaluationResult {
			continue
		}

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

	// login
	loginIfRequired(ctx, credentials, dedupedStages...)

	// pull all images in parallel
	for _, p := range dedupedStages {
		go func(p manifest.EstafetteStage) {
			defer wg.Done()
			log.Info().Msgf("Pulling container image %v\n", p.ContainerImage)
			pullArgs := []string{
				"pull",
				p.ContainerImage,
			}
			foundation.RunCommandWithArgsExtended(ctx, "docker", pullArgs)
		}(*p)
	}

	// wait for all pulls to finish
	wg.Wait()
	prefetchDuration := time.Since(prefetchStart)

	log.Info().Msgf("Done prefetching %v images in %v seconds", len(dedupedStages), prefetchDuration.Seconds())
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

func loginIfRequired(ctx context.Context, credentials []ContainerRegistryCredentials, stages ...*manifest.EstafetteStage) {

	containerImages := []string{}
	for _, s := range stages {
		containerImages = append(containerImages, s.ContainerImage)
	}

	log.Info().Msgf("Filtering credentials for images %v\n", containerImages)

	// retrieve all credentials
	filteredCredentialsMap := getCredentialsForContainers(credentials, containerImages)

	log.Info().Msgf("Filtered %v container-registry credentials down to %v\n", len(credentials), len(filteredCredentialsMap))

	if filteredCredentialsMap != nil {
		for _, c := range filteredCredentialsMap {
			if c != nil {
				log.Info().Msgf("Logging in to repository '%v'\n", c.AdditionalProperties.Repository)
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

				err := foundation.RunCommandWithArgsExtended(ctx, "docker", loginArgs)
				foundation.HandleError(err)
			}
		}
	}
}

func evaluateWhen(pipelineName, input string, parameters map[string]interface{}) (result bool, err error) {

	if input == "" {
		return false, errors.New("When expression is empty")
	}

	expression, err := govaluate.NewEvaluableExpression(input)
	if err != nil {
		return
	}

	r, err := expression.Evaluate(parameters)

	if result, ok := r.(bool); ok {
		return result, err
	}

	return false, errors.New("Result of evaluating when expression is not of type boolean")
}

func getParameters() map[string]interface{} {

	parameters := make(map[string]interface{}, 3)
	parameters["branch"] = os.Getenv("ESTAFETTE_GIT_BRANCH")
	parameters["trigger"] = os.Getenv("ESTAFETTE_TRIGGER")
	parameters["status"] = os.Getenv("ESTAFETTE_BUILD_STATUS")
	parameters["action"] = os.Getenv("ESTAFETTE_RELEASE_ACTION")
	parameters["server"] = "estafette"

	return parameters
}
