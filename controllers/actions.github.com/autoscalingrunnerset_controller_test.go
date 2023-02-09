package actionsgithubcom

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/actions/actions-runner-controller/apis/actions.github.com/v1alpha1"
	"github.com/actions/actions-runner-controller/github/actions"
	"github.com/actions/actions-runner-controller/github/actions/fake"
	"github.com/actions/actions-runner-controller/github/actions/testserver"
)

const (
	autoscalingRunnerSetTestTimeout     = time.Second * 5
	autoscalingRunnerSetTestInterval    = time.Millisecond * 250
	autoscalingRunnerSetTestGitHubToken = "gh_token"
)

var _ = Describe("Test AutoScalingRunnerSet controller", func() {
	var ctx context.Context
	var cancel context.CancelFunc
	autoscalingNS := new(corev1.Namespace)
	autoscalingRunnerSet := new(v1alpha1.AutoscalingRunnerSet)
	configSecret := new(corev1.Secret)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.TODO())
		autoscalingNS = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "testns-autoscaling" + RandStringRunes(5)},
		}

		err := k8sClient.Create(ctx, autoscalingNS)
		Expect(err).NotTo(HaveOccurred(), "failed to create test namespace for AutoScalingRunnerSet")

		configSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "github-config-secret",
				Namespace: autoscalingNS.Name,
			},
			Data: map[string][]byte{
				"github_token": []byte(autoscalingRunnerSetTestGitHubToken),
			},
		}

		err = k8sClient.Create(ctx, configSecret)
		Expect(err).NotTo(HaveOccurred(), "failed to create config secret")

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Namespace:          autoscalingNS.Name,
			MetricsBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create manager")

		controller := &AutoscalingRunnerSetReconciler{
			Client:                             mgr.GetClient(),
			Scheme:                             mgr.GetScheme(),
			Log:                                logf.Log,
			ControllerNamespace:                autoscalingNS.Name,
			DefaultRunnerScaleSetListenerImage: "ghcr.io/actions/arc",
			ActionsClient:                      fake.NewMultiClient(),
		}
		err = controller.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred(), "failed to setup controller")

		min := 1
		max := 10
		autoscalingRunnerSet = &v1alpha1.AutoscalingRunnerSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-asrs",
				Namespace: autoscalingNS.Name,
			},
			Spec: v1alpha1.AutoscalingRunnerSetSpec{
				GitHubConfigUrl:    "https://github.com/owner/repo",
				GitHubConfigSecret: configSecret.Name,
				MaxRunners:         &max,
				MinRunners:         &min,
				RunnerGroup:        "testgroup",
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "runner",
								Image: "ghcr.io/actions/runner",
							},
						},
					},
				},
			},
		}

		err = k8sClient.Create(ctx, autoscalingRunnerSet)
		Expect(err).NotTo(HaveOccurred(), "failed to create AutoScalingRunnerSet")

		go func() {
			defer GinkgoRecover()

			err := mgr.Start(ctx)
			Expect(err).NotTo(HaveOccurred(), "failed to start manager")
		}()
	})

	AfterEach(func() {
		defer cancel()

		err := k8sClient.Delete(ctx, autoscalingNS)
		Expect(err).NotTo(HaveOccurred(), "failed to delete test namespace for AutoScalingRunnerSet")
	})

	Context("When creating a new AutoScalingRunnerSet", func() {
		It("It should create/add all required resources for a new AutoScalingRunnerSet (finalizer, runnerscaleset, ephemeralrunnerset, listener)", func() {
			// Check if finalizer is added
			created := new(v1alpha1.AutoscalingRunnerSet)
			Eventually(
				func() (string, error) {
					err := k8sClient.Get(ctx, client.ObjectKey{Name: autoscalingRunnerSet.Name, Namespace: autoscalingRunnerSet.Namespace}, created)
					if err != nil {
						return "", err
					}
					if len(created.Finalizers) == 0 {
						return "", nil
					}
					return created.Finalizers[0], nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(BeEquivalentTo(autoscalingRunnerSetFinalizerName), "AutoScalingRunnerSet should have a finalizer")

			// Check if runner scale set is created on service
			Eventually(
				func() (string, error) {
					err := k8sClient.Get(ctx, client.ObjectKey{Name: autoscalingRunnerSet.Name, Namespace: autoscalingRunnerSet.Namespace}, created)
					if err != nil {
						return "", err
					}

					if _, ok := created.Annotations[runnerScaleSetIdKey]; !ok {
						return "", nil
					}

					if _, ok := created.Annotations[runnerScaleSetRunnerGroupNameKey]; !ok {
						return "", nil
					}

					return fmt.Sprintf("%s_%s", created.Annotations[runnerScaleSetIdKey], created.Annotations[runnerScaleSetRunnerGroupNameKey]), nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(BeEquivalentTo("1_testgroup"), "RunnerScaleSet should be created/fetched and update the AutoScalingRunnerSet's annotation")

			// Check if ephemeral runner set is created
			Eventually(
				func() (int, error) {
					runnerSetList := new(v1alpha1.EphemeralRunnerSetList)
					err := k8sClient.List(ctx, runnerSetList, client.InNamespace(autoscalingRunnerSet.Namespace))
					if err != nil {
						return 0, err
					}

					return len(runnerSetList.Items), nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(BeEquivalentTo(1), "Only one EphemeralRunnerSet should be created")

			// Check if listener is created
			Eventually(
				func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Name: scaleSetListenerName(autoscalingRunnerSet), Namespace: autoscalingRunnerSet.Namespace}, new(v1alpha1.AutoscalingListener))
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(Succeed(), "Listener should be created")

			// Check if status is updated
			runnerSetList := new(v1alpha1.EphemeralRunnerSetList)
			err := k8sClient.List(ctx, runnerSetList, client.InNamespace(autoscalingRunnerSet.Namespace))
			Expect(err).NotTo(HaveOccurred(), "failed to list EphemeralRunnerSet")
			Expect(len(runnerSetList.Items)).To(BeEquivalentTo(1), "Only one EphemeralRunnerSet should be created")
			runnerSet := runnerSetList.Items[0]
			statusUpdate := runnerSet.DeepCopy()
			statusUpdate.Status.CurrentReplicas = 100
			err = k8sClient.Status().Patch(ctx, statusUpdate, client.MergeFrom(&runnerSet))
			Expect(err).NotTo(HaveOccurred(), "failed to patch EphemeralRunnerSet status")

			Eventually(
				func() (int, error) {
					updated := new(v1alpha1.AutoscalingRunnerSet)
					err := k8sClient.Get(ctx, client.ObjectKey{Name: autoscalingRunnerSet.Name, Namespace: autoscalingRunnerSet.Namespace}, updated)
					if err != nil {
						return 0, fmt.Errorf("failed to get AutoScalingRunnerSet: %w", err)
					}
					return updated.Status.CurrentRunners, nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(BeEquivalentTo(100), "AutoScalingRunnerSet status should be updated")
		})
	})

	Context("When deleting a new AutoScalingRunnerSet", func() {
		It("It should cleanup all resources for a deleting AutoScalingRunnerSet before removing it", func() {
			// Wait till the listener is created
			Eventually(
				func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Name: scaleSetListenerName(autoscalingRunnerSet), Namespace: autoscalingRunnerSet.Namespace}, new(v1alpha1.AutoscalingListener))
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(Succeed(), "Listener should be created")

			// Delete the AutoScalingRunnerSet
			err := k8sClient.Delete(ctx, autoscalingRunnerSet)
			Expect(err).NotTo(HaveOccurred(), "failed to delete AutoScalingRunnerSet")

			// Check if the listener is deleted
			Eventually(
				func() error {
					err := k8sClient.Get(ctx, client.ObjectKey{Name: scaleSetListenerName(autoscalingRunnerSet), Namespace: autoscalingRunnerSet.Namespace}, new(v1alpha1.AutoscalingListener))
					if err != nil && errors.IsNotFound(err) {
						return nil
					}

					return fmt.Errorf("listener is not deleted")
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(Succeed(), "Listener should be deleted")

			// Check if all the EphemeralRunnerSet is deleted
			Eventually(
				func() error {
					runnerSetList := new(v1alpha1.EphemeralRunnerSetList)
					err := k8sClient.List(ctx, runnerSetList, client.InNamespace(autoscalingRunnerSet.Namespace))
					if err != nil {
						return err
					}

					if len(runnerSetList.Items) != 0 {
						return fmt.Errorf("EphemeralRunnerSet is not deleted, count=%v", len(runnerSetList.Items))
					}

					return nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(Succeed(), "All EphemeralRunnerSet should be deleted")

			// Check if the AutoScalingRunnerSet is deleted
			Eventually(
				func() error {
					err := k8sClient.Get(ctx, client.ObjectKey{Name: autoscalingRunnerSet.Name, Namespace: autoscalingRunnerSet.Namespace}, new(v1alpha1.AutoscalingRunnerSet))
					if err != nil && errors.IsNotFound(err) {
						return nil
					}

					return fmt.Errorf("AutoScalingRunnerSet is not deleted")
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(Succeed(), "AutoScalingRunnerSet should be deleted")
		})
	})

	Context("When updating a new AutoScalingRunnerSet", func() {
		It("It should re-create EphemeralRunnerSet and Listener as needed when updating AutoScalingRunnerSet", func() {
			// Wait till the listener is created
			listener := new(v1alpha1.AutoscalingListener)
			Eventually(
				func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Name: scaleSetListenerName(autoscalingRunnerSet), Namespace: autoscalingRunnerSet.Namespace}, listener)
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(Succeed(), "Listener should be created")

			runnerSetList := new(v1alpha1.EphemeralRunnerSetList)
			err := k8sClient.List(ctx, runnerSetList, client.InNamespace(autoscalingRunnerSet.Namespace))
			Expect(err).NotTo(HaveOccurred(), "failed to list EphemeralRunnerSet")
			Expect(len(runnerSetList.Items)).To(Equal(1), "There should be 1 EphemeralRunnerSet")
			runnerSet := runnerSetList.Items[0]

			// Update the AutoScalingRunnerSet.Spec.Template
			// This should trigger re-creation of EphemeralRunnerSet and Listener
			patched := autoscalingRunnerSet.DeepCopy()
			patched.Spec.Template.Spec.PriorityClassName = "test-priority-class"
			err = k8sClient.Patch(ctx, patched, client.MergeFrom(autoscalingRunnerSet))
			Expect(err).NotTo(HaveOccurred(), "failed to patch AutoScalingRunnerSet")
			autoscalingRunnerSet = patched.DeepCopy()

			// We should create a new EphemeralRunnerSet and delete the old one, eventually, we will have only one EphemeralRunnerSet
			Eventually(
				func() (string, error) {
					runnerSetList := new(v1alpha1.EphemeralRunnerSetList)
					err := k8sClient.List(ctx, runnerSetList, client.InNamespace(autoscalingRunnerSet.Namespace))
					if err != nil {
						return "", err
					}

					if len(runnerSetList.Items) != 1 {
						return "", fmt.Errorf("We should have only 1 EphemeralRunnerSet, but got %v", len(runnerSetList.Items))
					}

					return runnerSetList.Items[0].Labels[LabelKeyRunnerSpecHash], nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).ShouldNot(BeEquivalentTo(runnerSet.Labels[LabelKeyRunnerSpecHash]), "New EphemeralRunnerSet should be created")

			// We should create a new listener
			Eventually(
				func() (string, error) {
					listener := new(v1alpha1.AutoscalingListener)
					err := k8sClient.Get(ctx, client.ObjectKey{Name: scaleSetListenerName(autoscalingRunnerSet), Namespace: autoscalingRunnerSet.Namespace}, listener)
					if err != nil {
						return "", err
					}

					return listener.Spec.EphemeralRunnerSetName, nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).ShouldNot(BeEquivalentTo(runnerSet.Name), "New Listener should be created")

			// Only update the Spec for the AutoScalingListener
			// This should trigger re-creation of the Listener only
			runnerSetList = new(v1alpha1.EphemeralRunnerSetList)
			err = k8sClient.List(ctx, runnerSetList, client.InNamespace(autoscalingRunnerSet.Namespace))
			Expect(err).NotTo(HaveOccurred(), "failed to list EphemeralRunnerSet")
			Expect(len(runnerSetList.Items)).To(Equal(1), "There should be 1 EphemeralRunnerSet")
			runnerSet = runnerSetList.Items[0]

			listener = new(v1alpha1.AutoscalingListener)
			err = k8sClient.Get(ctx, client.ObjectKey{Name: scaleSetListenerName(autoscalingRunnerSet), Namespace: autoscalingRunnerSet.Namespace}, listener)
			Expect(err).NotTo(HaveOccurred(), "failed to get Listener")

			patched = autoscalingRunnerSet.DeepCopy()
			min := 10
			patched.Spec.MinRunners = &min
			err = k8sClient.Patch(ctx, patched, client.MergeFrom(autoscalingRunnerSet))
			Expect(err).NotTo(HaveOccurred(), "failed to patch AutoScalingRunnerSet")

			// We should not re-create a new EphemeralRunnerSet
			Consistently(
				func() (string, error) {
					runnerSetList := new(v1alpha1.EphemeralRunnerSetList)
					err := k8sClient.List(ctx, runnerSetList, client.InNamespace(autoscalingRunnerSet.Namespace))
					if err != nil {
						return "", err
					}

					if len(runnerSetList.Items) != 1 {
						return "", fmt.Errorf("We should have only 1 EphemeralRunnerSet, but got %v", len(runnerSetList.Items))
					}

					return string(runnerSetList.Items[0].UID), nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(BeEquivalentTo(string(runnerSet.UID)), "New EphemeralRunnerSet should not be created")

			// We should only re-create a new listener
			Eventually(
				func() (string, error) {
					listener := new(v1alpha1.AutoscalingListener)
					err := k8sClient.Get(ctx, client.ObjectKey{Name: scaleSetListenerName(autoscalingRunnerSet), Namespace: autoscalingRunnerSet.Namespace}, listener)
					if err != nil {
						return "", err
					}

					return string(listener.UID), nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).ShouldNot(BeEquivalentTo(string(listener.UID)), "New Listener should be created")
		})

		It("It should update RunnerScaleSet's runner group on service when it changes", func() {
			updated := new(v1alpha1.AutoscalingRunnerSet)
			// Wait till the listener is created
			Eventually(
				func() error {
					return k8sClient.Get(ctx, client.ObjectKey{Name: scaleSetListenerName(autoscalingRunnerSet), Namespace: autoscalingRunnerSet.Namespace}, new(v1alpha1.AutoscalingListener))
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(Succeed(), "Listener should be created")

			patched := autoscalingRunnerSet.DeepCopy()
			patched.Spec.RunnerGroup = "testgroup2"
			err := k8sClient.Patch(ctx, patched, client.MergeFrom(autoscalingRunnerSet))
			Expect(err).NotTo(HaveOccurred(), "failed to patch AutoScalingRunnerSet")

			// Check if AutoScalingRunnerSet has the new runner group in its annotation
			Eventually(
				func() (string, error) {
					err := k8sClient.Get(ctx, client.ObjectKey{Name: autoscalingRunnerSet.Name, Namespace: autoscalingRunnerSet.Namespace}, updated)
					if err != nil {
						return "", err
					}

					if _, ok := updated.Annotations[runnerScaleSetRunnerGroupNameKey]; !ok {
						return "", nil
					}

					return updated.Annotations[runnerScaleSetRunnerGroupNameKey], nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(BeEquivalentTo("testgroup2"), "AutoScalingRunnerSet should have the new runner group in its annotation")

			// delete the annotation and it should be re-added
			patched = autoscalingRunnerSet.DeepCopy()
			delete(patched.Annotations, runnerScaleSetRunnerGroupNameKey)
			err = k8sClient.Patch(ctx, patched, client.MergeFrom(autoscalingRunnerSet))
			Expect(err).NotTo(HaveOccurred(), "failed to patch AutoScalingRunnerSet")

			// Check if AutoScalingRunnerSet still has the runner group in its annotation
			Eventually(
				func() (string, error) {
					err := k8sClient.Get(ctx, client.ObjectKey{Name: autoscalingRunnerSet.Name, Namespace: autoscalingRunnerSet.Namespace}, updated)
					if err != nil {
						return "", err
					}

					if _, ok := updated.Annotations[runnerScaleSetRunnerGroupNameKey]; !ok {
						return "", nil
					}

					return updated.Annotations[runnerScaleSetRunnerGroupNameKey], nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval).Should(BeEquivalentTo("testgroup2"), "AutoScalingRunnerSet should have the runner group in its annotation")
		})
	})
})

var _ = Describe("Test AutoscalingController creation failures", func() {
	Context("When autoscaling runner set creation fails on the client", func() {
		var ctx context.Context
		var cancel context.CancelFunc
		autoscalingNS := new(corev1.Namespace)
		configSecret := new(corev1.Secret)

		BeforeEach(func() {
			ctx, cancel = context.WithCancel(context.TODO())
			autoscalingNS = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "testns-autoscaling" + RandStringRunes(5)},
			}

			err := k8sClient.Create(ctx, autoscalingNS)
			Expect(err).NotTo(HaveOccurred(), "failed to create test namespace for AutoScalingRunnerSet")

			configSecret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "github-config-secret",
					Namespace: autoscalingNS.Name,
				},
				Data: map[string][]byte{
					"github_token": []byte(autoscalingRunnerSetTestGitHubToken),
				},
			}

			err = k8sClient.Create(ctx, configSecret)
			Expect(err).NotTo(HaveOccurred(), "failed to create config secret")

			mgr, err := ctrl.NewManager(cfg, ctrl.Options{})
			Expect(err).NotTo(HaveOccurred(), "failed to create manager")

			controller := &AutoscalingRunnerSetReconciler{
				Client:                             mgr.GetClient(),
				Scheme:                             mgr.GetScheme(),
				Log:                                logf.Log,
				ControllerNamespace:                autoscalingNS.Name,
				DefaultRunnerScaleSetListenerImage: "ghcr.io/actions/arc",
				ActionsClient:                      fake.NewMultiClient(),
			}
			err = controller.SetupWithManager(mgr)
			Expect(err).NotTo(HaveOccurred(), "failed to setup controller")

			go func() {
				defer GinkgoRecover()

				err := mgr.Start(ctx)
				Expect(err).NotTo(HaveOccurred(), "failed to start manager")
			}()
		})

		AfterEach(func() {
			defer cancel()

			err := k8sClient.Delete(ctx, autoscalingNS)
			Expect(err).NotTo(HaveOccurred(), "failed to delete test namespace for AutoScalingRunnerSet")
		})

		It("It should be able to clean up if annotation related to scale set id does not exist", func() {
			min := 1
			max := 10
			autoscalingRunnerSet := &v1alpha1.AutoscalingRunnerSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-asrs",
					Namespace: autoscalingNS.Name,
				},
				Spec: v1alpha1.AutoscalingRunnerSetSpec{
					GitHubConfigUrl: "https://github.com/owner/repo",
					MaxRunners:      &max,
					MinRunners:      &min,
					RunnerGroup:     "testgroup",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "runner",
									Image: "ghcr.io/actions/runner",
								},
							},
						},
					},
				},
			}

			err := k8sClient.Create(ctx, autoscalingRunnerSet)
			Expect(err).NotTo(HaveOccurred(), "failed to create AutoScalingRunnerSet")

			// wait for the finalizer to be added
			ars := new(v1alpha1.AutoscalingRunnerSet)
			Eventually(
				func() (string, error) {
					err := k8sClient.Get(ctx, client.ObjectKey{Name: autoscalingRunnerSet.Name, Namespace: autoscalingRunnerSet.Namespace}, ars)
					if err != nil {
						return "", err
					}
					if len(ars.Finalizers) == 0 {
						return "", nil
					}
					return ars.Finalizers[0], nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval,
			).Should(BeEquivalentTo(autoscalingRunnerSetFinalizerName), "AutoScalingRunnerSet should have a finalizer")

			ars.ObjectMeta.Annotations = make(map[string]string)
			err = k8sClient.Update(ctx, ars)
			Expect(err).NotTo(HaveOccurred(), "Update autoscaling runner set without annotation should be successful")

			Eventually(
				func() (bool, error) {
					err := k8sClient.Get(ctx, client.ObjectKey{Name: autoscalingRunnerSet.Name, Namespace: autoscalingRunnerSet.Namespace}, ars)
					if err != nil {
						return false, err
					}
					return len(ars.ObjectMeta.Annotations) == 0, nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval,
			).Should(BeEquivalentTo(true), "Autoscaling runner set should be updated with empty annotations")

			err = k8sClient.Delete(ctx, ars)
			Expect(err).NotTo(HaveOccurred(), "Delete autoscaling runner set should be successful")

			Eventually(
				func() (bool, error) {
					updated := new(v1alpha1.AutoscalingRunnerSet)
					err := k8sClient.Get(ctx, client.ObjectKey{Name: autoscalingRunnerSet.Name, Namespace: autoscalingRunnerSet.Namespace}, updated)
					if err == nil {
						return false, nil
					}
					if !errors.IsNotFound(err) {
						return false, err
					}

					return !controllerutil.ContainsFinalizer(updated, autoscalingRunnerSetFinalizerName), nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingRunnerSetTestInterval,
			).Should(BeEquivalentTo(true), "Finalizer and resource should eventually be deleted")
		})
	})
})

var _ = Describe("Test Client optional configuration", func() {
	Context("When specifying a configmap for root CAs", func() {
		var ctx context.Context
		var cancel context.CancelFunc
		autoscalingNS := new(corev1.Namespace)
		configSecret := new(corev1.Secret)
		rootCAConfigMap := new(corev1.ConfigMap)
		var mgr ctrl.Manager

		BeforeEach(func() {
			ctx, cancel = context.WithCancel(context.TODO())
			autoscalingNS = &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "testns-autoscaling" + RandStringRunes(5)},
			}

			err := k8sClient.Create(ctx, autoscalingNS)
			Expect(err).NotTo(HaveOccurred(), "failed to create test namespace for AutoScalingRunnerSet")

			configSecret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "github-config-secret",
					Namespace: autoscalingNS.Name,
				},
				Data: map[string][]byte{
					"github_token": []byte(autoscalingRunnerSetTestGitHubToken),
				},
			}

			err = k8sClient.Create(ctx, configSecret)
			Expect(err).NotTo(HaveOccurred(), "failed to create config secret")

			cert, err := os.ReadFile(filepath.Join(
				"../../",
				"github",
				"actions",
				"testdata",
				"rootCA.crt",
			))
			Expect(err).NotTo(HaveOccurred(), "failed to read root CA cert")
			rootCAConfigMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "root-ca-configmap",
					Namespace: autoscalingNS.Name,
				},
				BinaryData: map[string][]byte{
					"rootCA.crt": cert,
				},
			}
			err = k8sClient.Create(ctx, rootCAConfigMap)
			Expect(err).NotTo(HaveOccurred(), "failed to create configmap with root CAs")

			mgr, err = ctrl.NewManager(cfg, ctrl.Options{
				Namespace: autoscalingNS.Name,
			})
			Expect(err).NotTo(HaveOccurred(), "failed to create manager")

			go func() {
				defer GinkgoRecover()

				err := mgr.Start(ctx)
				Expect(err).NotTo(HaveOccurred(), "failed to start manager")
			}()
		})

		AfterEach(func() {
			defer cancel()

			err := k8sClient.Delete(ctx, autoscalingNS)
			Expect(err).NotTo(HaveOccurred(), "failed to delete test namespace for AutoScalingRunnerSet")
		})

		It("should be able to make requests to a server using root CAs", func() {
			controller := &AutoscalingRunnerSetReconciler{
				Client:                             mgr.GetClient(),
				Scheme:                             mgr.GetScheme(),
				Log:                                logf.Log,
				ControllerNamespace:                autoscalingNS.Name,
				DefaultRunnerScaleSetListenerImage: "ghcr.io/actions/arc",
				ActionsClient:                      actions.NewMultiClient("test", logr.Discard()),
			}
			err := controller.SetupWithManager(mgr)
			Expect(err).NotTo(HaveOccurred(), "failed to setup controller")

			certsFolder := filepath.Join(
				"../../",
				"github",
				"actions",
				"testdata",
			)
			certPath := filepath.Join(certsFolder, "server.crt")
			keyPath := filepath.Join(certsFolder, "server.key")

			serverSuccessfullyCalled := false
			server := testserver.NewUnstarted(GinkgoT(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serverSuccessfullyCalled = true
				w.WriteHeader(http.StatusOK)
			}))
			cert, err := tls.LoadX509KeyPair(certPath, keyPath)
			Expect(err).NotTo(HaveOccurred(), "failed to load server cert")

			server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
			server.StartTLS()

			min := 1
			max := 10
			autoscalingRunnerSet := &v1alpha1.AutoscalingRunnerSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-asrs",
					Namespace: autoscalingNS.Name,
				},
				Spec: v1alpha1.AutoscalingRunnerSetSpec{
					GitHubConfigUrl:    server.ConfigURLForOrg("my-org"),
					GitHubConfigSecret: configSecret.Name,
					GitHubServerTLS: &v1alpha1.GitHubServerTLSConfig{
						RootCAsConfigMapRef: rootCAConfigMap.Name,
					},
					MaxRunners:  &max,
					MinRunners:  &min,
					RunnerGroup: "testgroup",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "runner",
									Image: "ghcr.io/actions/runner",
								},
							},
						},
					},
				},
			}

			err = k8sClient.Create(ctx, autoscalingRunnerSet)
			Expect(err).NotTo(HaveOccurred(), "failed to create AutoScalingRunnerSet")

			// wait for server to be called
			Eventually(
				func() (bool, error) {
					return serverSuccessfullyCalled, nil
				},
				autoscalingRunnerSetTestTimeout,
				1*time.Nanosecond,
			).Should(BeTrue(), "server was not called")
		})

		It("it creates a listener referencing the right configmap for TLS", func() {
			controller := &AutoscalingRunnerSetReconciler{
				Client:                             mgr.GetClient(),
				Scheme:                             mgr.GetScheme(),
				Log:                                logf.Log,
				ControllerNamespace:                autoscalingNS.Name,
				DefaultRunnerScaleSetListenerImage: "ghcr.io/actions/arc",
				ActionsClient:                      fake.NewMultiClient(),
			}
			err := controller.SetupWithManager(mgr)
			Expect(err).NotTo(HaveOccurred(), "failed to setup controller")

			min := 1
			max := 10
			autoscalingRunnerSet := &v1alpha1.AutoscalingRunnerSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-asrs",
					Namespace: autoscalingNS.Name,
				},
				Spec: v1alpha1.AutoscalingRunnerSetSpec{
					GitHubConfigUrl:    "https://github.com/owner/repo",
					GitHubConfigSecret: configSecret.Name,
					GitHubServerTLS: &v1alpha1.GitHubServerTLSConfig{
						RootCAsConfigMapRef: rootCAConfigMap.Name,
					},
					MaxRunners:  &max,
					MinRunners:  &min,
					RunnerGroup: "testgroup",
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "runner",
									Image: "ghcr.io/actions/runner",
								},
							},
						},
					},
				},
			}

			err = k8sClient.Create(ctx, autoscalingRunnerSet)
			Expect(err).NotTo(HaveOccurred(), "failed to create AutoScalingRunnerSet")

			Eventually(
				func() (string, error) {
					listener := new(v1alpha1.AutoscalingListener)
					err := k8sClient.Get(
						ctx,
						client.ObjectKey{
							Name:      scaleSetListenerName(autoscalingRunnerSet),
							Namespace: autoscalingRunnerSet.Namespace,
						},
						listener,
					)
					if err != nil {
						return "", err
					}

					if listener.Spec.GitHubServerTLS == nil {
						return "", nil
					}

					return listener.Spec.GitHubServerTLS.RootCAsConfigMapRef, nil
				},
				autoscalingRunnerSetTestTimeout,
				autoscalingListenerTestInterval,
			).Should(BeEquivalentTo(rootCAConfigMap.Name), "configmap reference is incorrect")
		})
	})
})
