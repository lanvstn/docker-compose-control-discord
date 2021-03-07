package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

const (
	dockerComposeProjectLabel = "com.docker.compose.project"
	dockerComposeServiceLabel = "com.docker.compose.service"
)

func startServer() error {
	return runCommand("docker-compose", "up", "-d")
}

func stopServer() error {
	return runCommand("docker-compose", "down")
}

func getStatus() bool {
	if testMode {
		return false
	}

	out, err := exec.Command("docker-compose", "top").CombinedOutput()
	if err != nil {
		log.Printf("failed to get status: %s\n", err.Error())
		return false
	}

	if strings.TrimSpace(string(out)) == "" {
		return false
	}

	return true
}

func appContainer(ctx context.Context, dcli *client.Client, projectName, serviceName string) (*types.Container, error) {
	all, err := dcli.ContainerList(ctx, types.ContainerListOptions{}) // TODO filter directly on docker using filter args
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	for _, container := range all {
		matchedProject := false
		matchedService := false
		if projectLabel, ok := container.Labels[dockerComposeProjectLabel]; ok && projectLabel == projectName {
			matchedProject = true
		}
		if serviceLabel, ok := container.Labels[dockerComposeServiceLabel]; ok && serviceLabel == serviceName {
			matchedService = true
		}

		if matchedProject && matchedService {
			return &container, nil
		}
	}

	return nil, errors.New("no matching container found")
}
