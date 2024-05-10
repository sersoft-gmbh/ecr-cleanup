package awshelpers

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrTypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"k8s.io/utils/strings/slices"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const (
	paginationLimit   = 100
	deletionBatchSize = 100
	latestTag         = "latest"
)

type AWSDockerImage interface {
	GetRegistry() string
	GetName() string
	GetTag() string
}

type compiledConfig struct {
	accountId       string
	repositoryRegex *regexp.Regexp
}

func (config Config) compile() (compiledConfig, error) {
	var repoRegex *regexp.Regexp
	if config.RepositoryRegex != "" {
		var err error
		repoRegex, err = regexp.Compile(config.RepositoryRegex)
		if err != nil {
			return compiledConfig{}, err
		}
	}
	return compiledConfig{
		accountId:       config.AccountId,
		repositoryRegex: repoRegex,
	}, nil
}

func relevantRepositories(ctx context.Context, config compiledConfig, client *ecr.Client) ([]ecrTypes.Repository, error) {
	var registryId *string
	if config.accountId != "" {
		registryId = &config.accountId
	}
	paginator := ecr.NewDescribeRepositoriesPaginator(client, &ecr.DescribeRepositoriesInput{
		RegistryId: registryId,
	}, func(o *ecr.DescribeRepositoriesPaginatorOptions) { o.Limit = paginationLimit })

	relevantRepositories := make([]ecrTypes.Repository, 0)
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, repo := range output.Repositories {
			if config.repositoryRegex == nil || config.repositoryRegex.MatchString(*repo.RepositoryName) {
				relevantRepositories = append(relevantRepositories, repo)
			}
		}
	}
	return relevantRepositories, nil
}

func repositoryImages(ctx context.Context, client *ecr.Client, repo ecrTypes.Repository) ([]ecrTypes.ImageDetail, error) {
	paginator := ecr.NewDescribeImagesPaginator(client, &ecr.DescribeImagesInput{
		RegistryId:     repo.RegistryId,
		RepositoryName: repo.RepositoryName,
	}, func(o *ecr.DescribeImagesPaginatorOptions) { o.Limit = paginationLimit })

	images := make([]ecrTypes.ImageDetail, 0)
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		images = append(images, output.ImageDetails...)
	}
	sort.Slice(images, func(i, j int) bool {
		return images[i].ImagePushedAt.Before(*images[j].ImagePushedAt)
	})
	return images, nil
}

func deletionData(repo ecrTypes.Repository, images []ecrTypes.ImageDetail, imagesToKeep []AWSDockerImage) (hashes []string, tags []string) {
	imageHashesToDelete := make([]string, 0)
	imageTagsThatWillBeDeleted := make([]string, 0)
	for _, image := range images {
		if slices.Contains(image.ImageTags, latestTag) || image.ImageDigest == nil || *image.ImageDigest == "" || slices.Contains(imageHashesToDelete, *image.ImageDigest) {
			continue
		}
		keep := false
		for _, imageToKeep := range imagesToKeep {
			if imageNameWithRepo := imageToKeep.GetRegistry() + "/" + imageToKeep.GetName(); imageNameWithRepo == *repo.RepositoryUri && slices.Contains(image.ImageTags, imageToKeep.GetTag()) {
				keep = true
				break
			}
		}
		if !keep {
			imageHashesToDelete = append(imageHashesToDelete, *image.ImageDigest)
			for _, tag := range image.ImageTags {
				if !slices.Contains(imageTagsThatWillBeDeleted, tag) {
					imageTagsThatWillBeDeleted = append(imageTagsThatWillBeDeleted, tag)
				}
			}
		}
	}
	return imageHashesToDelete, imageTagsThatWillBeDeleted
}

func deleteImages(ctx context.Context, client *ecr.Client, repo ecrTypes.Repository, imageHashesToDelete []string, dryRun bool) (int, error) {
	imageIdsToDelete := make([]ecrTypes.ImageIdentifier, len(imageHashesToDelete))
	// We need to iterate over indices here, because we borrow the pointer - which gets overridden in case of "range" iteration
	for i := 0; i < len(imageHashesToDelete); i++ {
		imageIdsToDelete[i] = ecrTypes.ImageIdentifier{ImageDigest: &imageHashesToDelete[i]}
	}
	totalCount := len(imageIdsToDelete)
	for i := 0; i < len(imageIdsToDelete); i += deletionBatchSize {
		end := min(i+deletionBatchSize, len(imageIdsToDelete))
		if dryRun {
			imageHashes := make([]string, end-i)
			for j, hash := range imageIdsToDelete[i:end] {
				imageHashes[j] = *hash.ImageDigest
			}
			println("Dry run: would delete the following image hashes in" + *repo.RepositoryName + ":")
			println(strings.Join(imageHashes, "\n"))
		} else {
			result, err := client.BatchDeleteImage(ctx, &ecr.BatchDeleteImageInput{
				ImageIds:       imageIdsToDelete[i:end],
				RepositoryName: repo.RepositoryName,
				RegistryId:     repo.RegistryId,
			})
			if err != nil {
				totalCount -= len(imageIdsToDelete[i:end])
				return totalCount, err
			}
			if len(result.Failures) > 0 {
				relevantFailures := make([]ecrTypes.ImageFailure, 0)
				uniqueFailures := make(map[string]bool)
				for _, failure := range result.Failures {
					// This is because we're pushing multi-arch images, and we might attempt to delete images that are still in use.
					// FIXME: This is a terrible hack, and we should find a better way to handle this.
					if failure.FailureCode != ecrTypes.ImageFailureCodeImageReferencedByManifestList {
						relevantFailures = append(relevantFailures, failure)
					}
					if _, ok := uniqueFailures[*failure.ImageId.ImageDigest]; !ok {
						totalCount--
						uniqueFailures[*failure.ImageId.ImageDigest] = true
					}
				}
				if len(relevantFailures) > 0 {
					failureMessage := "Failed to delete images in " + *repo.RepositoryName + ":"
					for _, failure := range relevantFailures {
						failureMessage += "\n" + *failure.ImageId.ImageDigest + " " + *failure.FailureReason
					}
					return totalCount, errors.New(failureMessage)
				}
			}
		}
	}
	return totalCount, nil
}

func CleanRepositories(ctx context.Context, awsConfig Config, imagesToKeep []AWSDockerImage) error {
	// Load the Shared AWS Configuration (~/.aws/config)
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}

	ecrClient := ecr.NewFromConfig(cfg)
	resolvedAWSConfig, err := awsConfig.compile()
	if err != nil {
		return err
	}

	relevantRepos, err := relevantRepositories(ctx, resolvedAWSConfig, ecrClient)
	if err != nil {
		return err
	}

	if len(relevantRepos) <= 0 {
		println("No repositories found to clean!")
		return nil
	}

	var wg sync.WaitGroup
	errorChannel := make(chan error, 1)
	sendError := func(errorChannel chan<- error, err error) {
		select {
		case errorChannel <- err:
		default:
			break
		}
	}

	wg.Add(len(relevantRepos))
	for _, repo := range relevantRepos {
		go func(repo ecrTypes.Repository, errorChannel chan<- error) {
			defer wg.Done()
			println("Processing repository", *repo.RepositoryUri)

			images, err := repositoryImages(ctx, ecrClient, repo)
			if err != nil {
				sendError(errorChannel, err)
				return
			}

			//for _, image := range images {
			//	println("Image in repository", *repo.RepositoryUri)
			//	println("Image digest:", *image.ImageDigest)
			//	println("Image tags:", strings.Join(image.ImageTags, ", "))
			//	println("Image pushed at:", image.ImagePushedAt.String())
			//	if image.ImageSizeInBytes != nil {
			//		println("Image size:", *image.ImageSizeInBytes)
			//	}
			//	if image.ArtifactMediaType != nil {
			//		println("Image artifact media type:", *image.ArtifactMediaType)
			//	}
			//	if image.ImageManifestMediaType != nil {
			//		println("Image manifest media type:", *image.ImageManifestMediaType)
			//	}
			//	println("")
			//}
			//return

			imageHashesToDelete, imageTagsThatWillBeDeleted := deletionData(repo, images, imagesToKeep)
			if len(imageHashesToDelete) <= 0 {
				println("No images to delete in", *repo.RepositoryUri)
				return
			}

			deletedCount, err := deleteImages(ctx, ecrClient, repo, imageHashesToDelete, awsConfig.DryRun)
			if err != nil {
				sendError(errorChannel, err)
				return
			}

			println("Deleted", deletedCount, "out of", len(imageHashesToDelete), "images from", *repo.RepositoryUri, "having the following tags:", strings.Join(imageTagsThatWillBeDeleted, ", "))
		}(repo, errorChannel)
	}

	wg.Wait()
	defer close(errorChannel)

	if len(errorChannel) > 0 {
		return <-errorChannel
	}

	return nil
}
