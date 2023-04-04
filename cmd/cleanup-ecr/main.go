package main

import (
	"context"
	"flag"
	"github.com/sersoft-gmbh/ecr-cleanup/internal/awshelpers"
	"github.com/sersoft-gmbh/ecr-cleanup/internal/kubehelpers"
	"strings"
)

type AWSDockerImg struct {
	registry string
	name     string
	tag      string
}

func (img AWSDockerImg) GetRegistry() string {
	return img.registry
}

func (img AWSDockerImg) GetName() string {
	return img.name
}

func (img AWSDockerImg) GetTag() string {
	return img.tag
}

func main() {
	kubeConfigPath := flag.String("kubeconfig", kubehelpers.KubeConfigDefaultPath(), "The path to the kubeconfig file")
	namespace := flag.String("namespace", "", "The namespace in Kubernetes")
	awsAccountId := flag.String("awsAccountId", "", "The AWS Account ID")
	repoRegex := flag.String("repositoryRegex", "", "Only matching ECR repositories will be cleaned up")
	dryRun := flag.Bool("dryRun", false, "If true, no images will be deleted")
	flag.Parse()

	k8sHelpersConfig := kubehelpers.Config{
		KubeConfigPath: *kubeConfigPath,
		Namespace:      *namespace,
	}
	awsHelpersConfig := awshelpers.Config{
		AccountId:       *awsAccountId,
		RepositoryRegex: *repoRegex,
		DryRun:          *dryRun,
	}

	ctx := context.Background()

	imagesInUse, err := kubehelpers.FindImagesInUse(ctx, k8sHelpersConfig)
	if err != nil {
		panic(err.Error())
	}

	awsImagesInUse := make([]awshelpers.AWSDockerImage, 0, len(imagesInUse))
	for _, img := range imagesInUse {
		// nil means it's a dockerhub image, not an AWS image
		// we only want to clean up AWS images, so also check for ECR in the registry
		if img.Registry != nil && strings.Contains(*img.Registry, ".ecr.") {
			awsImagesInUse = append(awsImagesInUse, AWSDockerImg{
				registry: *img.Registry,
				name:     img.Name,
				tag:      img.Tag,
			})
		}
	}

	err = awshelpers.CleanRepositories(ctx, awsHelpersConfig, awsImagesInUse)
	if err != nil {
		panic(err.Error())
	}
}
