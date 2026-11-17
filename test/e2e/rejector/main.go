// Command rejector is an NRI plugin for the e2e suite. It refuses the containers of the Pods that carry the label
// blockoverlayfs.io/e2e-reject=true, once the plugins before it have handled them.
package main

import (
	"context"
	"errors"
	"log"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
)

type rejector struct{}

func (rejector) CreateContainer(_ context.Context, pod *api.PodSandbox, _ *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	if pod.GetLabels()["blockoverlayfs.io/e2e-reject"] == "true" {
		return nil, nil, errors.New("refused for the e2e suite")
	}
	return nil, nil, nil
}

func main() {
	// containerd asks plugins in the order of their indexes: 90 comes after blockoverlayfs, at 10 by default.
	s, err := stub.New(rejector{}, stub.WithPluginName("e2e-rejector"), stub.WithPluginIdx("90"))
	if err != nil {
		log.Fatal(err)
	}
	// Run returns when containerd closes the connection. The suite stops the plugin by killing it.
	if err := s.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}
