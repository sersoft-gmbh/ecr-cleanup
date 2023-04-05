package kubehelpers

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"strings"
	"sync"
)

type DockerImage struct {
	Registry *string
	Name     string
	Tag      string
}

func imagesInPods(ctx context.Context, clients *kubernetes.Clientset, namespace string) ([]string, error) {
	list, err := clients.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	imagesInUse := make([]string, 0, len(list.Items))
	for _, pod := range list.Items {
		for _, status := range append(pod.Status.ContainerStatuses, append(pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses...)...) {
			if status.Ready || (status.Started != nil && *status.Started) || status.State.Running != nil || status.State.Waiting != nil {
				imagesInUse = append(imagesInUse, status.ImageID)
			}
		}
	}
	return imagesInUse, nil
}

func appendImagesInUseInSpec(imagesInUse *[]string, podSpec corev1.PodSpec) {
	for _, container := range append(podSpec.Containers, podSpec.InitContainers...) {
		*imagesInUse = append(*imagesInUse, container.Image)
	}
	for _, container := range podSpec.EphemeralContainers {
		*imagesInUse = append(*imagesInUse, container.Image)
	}
}

func imagesInCronJobs(ctx context.Context, clients *kubernetes.Clientset, namespace string) ([]string, error) {
	list, err := clients.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	imagesInUse := make([]string, 0, len(list.Items))
	for _, job := range list.Items {
		appendImagesInUseInSpec(&imagesInUse, job.Spec.JobTemplate.Spec.Template.Spec)
	}
	return imagesInUse, nil
}

func imagesInDeployments(ctx context.Context, clients *kubernetes.Clientset, namespace string) ([]string, error) {
	list, err := clients.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	imagesInUse := make([]string, 0, len(list.Items))
	for _, deployment := range list.Items {
		appendImagesInUseInSpec(&imagesInUse, deployment.Spec.Template.Spec)
	}
	return imagesInUse, nil
}

func FindImagesInUse(ctx context.Context, config Config) ([]DockerImage, error) {
	kubeConfig, err := clientcmd.BuildConfigFromFlags("", config.KubeConfigPath)
	if err != nil {
		return nil, err
	}

	clients, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}

	outputChannel := make(chan string)
	errChannel := make(chan error, 1)
	sendError := func(errorChannel chan<- error, err error) {
		select {
		case errorChannel <- err:
		default:
			break
		}
	}
	var wg sync.WaitGroup
	runJob := func(job func() ([]string, error)) {
		wg.Add(1)
		go func(errorChannel chan<- error) {
			defer wg.Done()
			images, err := job()
			if err != nil {
				sendError(errChannel, err)
			} else {
				for _, image := range images {
					outputChannel <- image
				}
			}
		}(errChannel)
	}
	runJob(func() ([]string, error) {
		return imagesInPods(ctx, clients, config.Namespace)
	})
	runJob(func() ([]string, error) {
		return imagesInCronJobs(ctx, clients, config.Namespace)
	})
	runJob(func() ([]string, error) {
		return imagesInDeployments(ctx, clients, config.Namespace)
	})
	go func() {
		wg.Wait()
		close(outputChannel)
	}()

	processedImageStrings := make(map[string]bool)
	imagesInUse := make([]DockerImage, 0)
	for image := range outputChannel {
		if _, ok := processedImageStrings[image]; ok {
			continue
		}
		imageParts := strings.SplitN(image, ":", 2)
		var registry *string
		var name string
		if strings.Contains(imageParts[0], ".") {
			registryParts := strings.SplitN(imageParts[0], "/", 2)
			registry = &registryParts[0]
			name = registryParts[1]
		} else {
			registry = nil
			name = imageParts[0]
		}
		imagesInUse = append(imagesInUse, DockerImage{
			Registry: registry,
			Name:     name,
			Tag:      imageParts[1],
		})
		processedImageStrings[image] = true
	}

	defer close(errChannel)

	if len(errChannel) > 0 {
		return nil, <-errChannel
	}

	return imagesInUse, nil
}
