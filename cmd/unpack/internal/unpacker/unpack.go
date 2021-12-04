package unpacker

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	olmv1alpha1 "github.com/joelanford/kuberpak/api/v1alpha1"
)

func UnpackCommand() *cobra.Command {
	var (
		unpacker     Unpacker
		manifestsDir string
	)

	cmd := &cobra.Command{
		Use:  "unpack",
		Args: cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := zap.New().WithValues("bundle", unpacker.BundleName)
			cl, err := getClient()
			if err != nil {
				log.Error(err, "could not get client")
				os.Exit(1)
			}
			unpacker.Client = cl
			unpacker.Manifests = os.DirFS(manifestsDir)
			unpacker.Log = log
			if err := unpacker.Run(cmd.Context()); err != nil {
				log.Error(err, "unpack failed")
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&unpacker.Namespace, "namespace", "", "namespace in which to unpack configmaps")
	cmd.Flags().StringVar(&unpacker.PodName, "pod-name", "", "name of pod with bundle image container")
	cmd.Flags().StringVar(&unpacker.BundleName, "bundle-name", "", "the name of the bundle object that is being unpacked")
	cmd.Flags().StringVar(&manifestsDir, "manifests-dir", "", "directory in which manifests can be found")
	return cmd
}

func getClient() (client.Client, error) {
	sch := scheme.Scheme
	if err := olmv1alpha1.AddToScheme(sch); err != nil {
		return nil, err
	}
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: sch})
}

type Unpacker struct {
	Log logr.Logger

	// Used to manage config maps and read a pod to get image digest
	Client    client.Client
	Namespace string
	PodName   string

	// A filesystem containing the bundle manifests
	Manifests fs.FS

	// Used to apply metadata to the generated configmaps
	PackageName string
	BundleName  string
}

func (u *Unpacker) Run(ctx context.Context) error {
	u.Log.Info("getting bundle")
	bundle := &olmv1alpha1.Bundle{}
	bundleKey := types.NamespacedName{Namespace: u.Namespace, Name: u.BundleName}
	if err := u.Client.Get(ctx, bundleKey, bundle); err != nil {
		return err
	}

	u.Log.Info("getting image digest")
	resolvedImage, err := u.getImageDigest(ctx, bundle.Spec.Image)
	if err != nil {
		return err
	}

	u.Log.Info("get objects")
	objects, err := u.getObjects()
	if err != nil {
		return err
	}

	var objectRefs []corev1.ObjectReference
	for _, obj := range objects {
		apiVersion, kind := obj.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
		objectRefs = append(objectRefs, corev1.ObjectReference{
			Kind:       kind,
			Namespace:  obj.GetNamespace(),
			Name:       obj.GetName(),
			APIVersion: apiVersion,
		})
	}

	u.Log.Info("get desired config maps")
	desiredConfigMaps, err := u.getDesiredConfigMaps(bundle, resolvedImage, objects)
	if err != nil {
		return err
	}

	u.Log.Info("get actual config maps")
	actualConfigMaps := &corev1.ConfigMapList{}
	if err := u.Client.List(ctx, actualConfigMaps, client.MatchingLabels(u.getConfigMapLabels()), client.InNamespace(u.Namespace)); err != nil {
		return err
	}

	u.Log.Info("ensure desired config maps")
	return u.ensureDesiredConfigMaps(ctx, actualConfigMaps.Items, desiredConfigMaps)
}

func (u *Unpacker) getImageDigest(ctx context.Context, image string) (string, error) {
	podKey := types.NamespacedName{Namespace: u.Namespace, Name: u.PodName}
	pod := &corev1.Pod{}
	if err := u.Client.Get(ctx, podKey, pod); err != nil {
		return "", err
	}
	for _, ps := range pod.Status.InitContainerStatuses {
		if ps.Image == image && ps.ImageID != "" {
			return ps.ImageID, nil
		}
	}
	for _, ps := range pod.Status.ContainerStatuses {
		if ps.Image == image && ps.ImageID != "" {
			return ps.ImageID, nil
		}
	}
	return "", fmt.Errorf("image digest for image %q not found", image)
}

func (u *Unpacker) getObjects() ([]client.Object, error) {
	var objects []client.Object

	entries, err := fs.ReadDir(u.Manifests, ".")
	if err != nil {
		return nil, fmt.Errorf("read manifests: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fileData, err := fs.ReadFile(u.Manifests, e.Name())
		if err != nil {
			return nil, err
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(fileData), 1024)
		for {
			obj := unstructured.Unstructured{}
			err := dec.Decode(&obj)
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, err
			}
			objects = append(objects, &obj)
		}
	}
	return objects, nil
}

func (u *Unpacker) getConfigMapLabels() map[string]string {
	return map[string]string{"kuberpak.io/bundle-name": u.BundleName}
}

func (u *Unpacker) getDesiredConfigMaps(bundle *olmv1alpha1.Bundle, resolvedImage string, objects []client.Object) ([]corev1.ConfigMap, error) {
	var desiredConfigMaps []corev1.ConfigMap
	for _, obj := range objects {
		objData, err := yaml.Marshal(obj)
		if err != nil {
			return nil, err
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(objData))
		objCompressed := &bytes.Buffer{}
		gzipper := gzip.NewWriter(objCompressed)
		if _, err := gzipper.Write(objData); err != nil {
			return nil, fmt.Errorf("gzip object data: %v", err)
		}
		if err := gzipper.Close(); err != nil {
			return nil, fmt.Errorf("close gzip writer: %v", err)
		}
		immutable := true
		kind, apiVersion := obj.GetObjectKind().GroupVersionKind().ToAPIVersionAndKind()
		cm := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("bundle-object-%s-%s", u.BundleName, hash[0:8]),
				Namespace: u.Namespace,
				Labels:    u.getConfigMapLabels(),
			},
			Immutable: &immutable,
			Data: map[string]string{
				"bundle-image":      resolvedImage,
				"object-sha256":     hash,
				"object-kind":       kind,
				"object-apiversion": apiVersion,
				"object-name":       obj.GetName(),
				"object-namespace":  obj.GetNamespace(),
			},
			BinaryData: map[string][]byte{
				"object": objCompressed.Bytes(),
			},
		}
		if err := controllerutil.SetControllerReference(bundle, &cm, u.Client.Scheme()); err != nil {
			return nil, fmt.Errorf("set owner reference on configmap: %v", err)
		}
		desiredConfigMaps = append(desiredConfigMaps, cm)
	}
	return desiredConfigMaps, nil
}

func (u *Unpacker) ensureDesiredConfigMaps(ctx context.Context, actual, desired []corev1.ConfigMap) error {
	actualCms := map[types.NamespacedName]corev1.ConfigMap{}
	for _, cm := range actual {
		key := types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}
		actualCms[key] = cm
	}

	for _, cm := range desired {
		cm := cm
		key := types.NamespacedName{Namespace: cm.Namespace, Name: cm.Name}
		if ecm, ok := actualCms[key]; ok {
			if stringMapsEqual(ecm.Labels, cm.Labels) &&
				stringMapsEqual(ecm.Annotations, cm.Annotations) &&
				stringMapsEqual(ecm.Data, cm.Data) &&
				bytesMapsEqual(ecm.BinaryData, cm.BinaryData) {
				delete(actualCms, key)
				continue
			}
			if err := u.Client.Delete(ctx, &ecm); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("delete configmap: %v", err)
			}
		}
		if err := u.Client.Create(ctx, &cm); err != nil {
			return fmt.Errorf("create configmap: %v", err)
		}
	}
	for _, ecm := range actualCms {
		if err := u.Client.Delete(ctx, &ecm); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete configmap: %v", err)
		}
	}
	return nil
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for ka, va := range a {
		vb, ok := b[ka]
		if !ok || va != vb {
			return false
		}
	}
	return true
}

func bytesMapsEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for ka, va := range a {
		vb, ok := b[ka]
		if !ok || !bytes.Equal(va, vb) {
			return false
		}
	}
	return true
}