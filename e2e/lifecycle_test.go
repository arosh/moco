package e2e

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	mocov1beta1 "github.com/cybozu-go/moco/api/v1beta1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/prometheus/common/expfmt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

//go:embed testdata/single.yaml
var singleYAML string

var _ = Context("lifecycle", func() {
	It("should construct a single-instance cluster", func() {
		kubectlSafe(fillTemplate(singleYAML), "apply", "-f", "-")
		Eventually(func() error {
			cluster, err := getCluster("foo", "single")
			if err != nil {
				return err
			}
			for _, cond := range cluster.Status.Conditions {
				if cond.Type != mocov1beta1.ConditionHealthy {
					continue
				}
				if cond.Status == corev1.ConditionTrue {
					return nil
				}
				return fmt.Errorf("cluster is not healthy: %s", cond.Status)
			}
			return errors.New("no health condition")
		}).Should(Succeed())
	})

	It("should log slow queries via sidecar", func() {
		out := kubectlSafe(nil, "moco", "-n", "foo", "mysql", "single", "--", "-N", "-e", "SELECT @@long_query_time")
		val, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		Expect(err).NotTo(HaveOccurred())
		Expect(val).To(BeNumerically("==", 0))

		Eventually(func() bool {
			out, err := kubectl(nil, "-n", "foo", "logs", "moco-single-0", "slow-log")
			if err != nil {
				return false
			}
			return strings.Contains(string(out), "# Query_time")
		}).Should(BeTrue())
	})

	It("should update the configmap and restart mysqld", func() {
		Eventually(func() error {
			cluster, err := getCluster("foo", "single")
			if err != nil {
				return err
			}
			cluster.Spec.MySQLConfigMapName = nil
			data, _ := json.Marshal(cluster)
			_, err = kubectl(data, "apply", "-f", "-")
			return err
		}).Should(Succeed())

		Eventually(func() float64 {
			out, err := kubectl(nil, "moco", "-n", "foo", "mysql", "single", "--", "-N", "-e", "SELECT @@long_query_time")
			if err != nil {
				return -1
			}
			val, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
			if err != nil {
				return -2
			}
			return val
		}).Should(BeNumerically("==", 2))
	})

	It("should allow writes via Service", func() {
		By("obtaining the credential")
		out := kubectlSafe(nil, "moco", "-n", "foo", "credential", "-u", "moco-writable", "single")
		passwd := strings.TrimSpace(string(out))

		By("running mysql in a pod")
		Eventually(func() error {
			_, err := runInPod("mysql", "-u", "moco-writable", "-p"+passwd,
				"-h", "moco-single-primary.foo.svc", "-e", "SELECT VERSION()")
			return err
		}).Should(Succeed())
		_, err := runInPod("mysql", "-u", "moco-writable", "-p"+passwd,
			"-h", "moco-single-primary.foo.svc", "-e", "CREATE DATABASE foo")
		Expect(err).NotTo(HaveOccurred())
	})

	It("should expose metrics", func() {
		out := kubectlSafe(nil, "-n", "moco-system", "get", "pods", "-l", "app.kubernetes.io/component=moco-controller", "-o", "json")
		pods := &corev1.PodList{}
		err := json.Unmarshal(out, pods)
		Expect(err).NotTo(HaveOccurred())
		Expect(pods.Items).To(HaveLen(1))
		addr := pods.Items[0].Status.PodIP
		out, err = runInPod("curl", "-sf", fmt.Sprintf("http://%s:8080/metrics", addr))
		Expect(err).NotTo(HaveOccurred())

		mfs, err := (&expfmt.TextParser{}).TextToMetricFamilies(bytes.NewReader(out))
		Expect(err).NotTo(HaveOccurred())
		mf := mfs["moco_cluster_replicas"]
		Expect(mf).NotTo(BeNil())
		m := findMetric(mf, map[string]string{"namespace": "foo", "name": "single"})
		Expect(m).NotTo(BeNil())
		Expect(m.GetGauge().GetValue()).To(BeNumerically("==", 1))
	})

	It("should collect generated resources after deleting MySQLCluster", func() {
		kubectlSafe(nil, "-n", "foo", "delete", "mysqlcluster", "single")
		Eventually(func() error {
			pvcs := &corev1.PersistentVolumeClaimList{}
			out, err := kubectl(nil, "-n", "foo", "get", "pvc", "-o", "json")
			if err != nil {
				return err
			}
			err = json.Unmarshal(out, pvcs)
			if err != nil {
				return err
			}
			if len(pvcs.Items) == 0 {
				return nil
			}
			return fmt.Errorf("pending pvcs: %+v", pvcs.Items)
		}).Should(Succeed())
		Eventually(func() error {
			stss := &appsv1.StatefulSetList{}
			out, err := kubectl(nil, "-n", "foo", "get", "statefulset", "-o", "json")
			if err != nil {
				return err
			}
			err = json.Unmarshal(out, stss)
			if err != nil {
				return err
			}
			if len(stss.Items) == 0 {
				return nil
			}
			return fmt.Errorf("pending stateful sets: %+v", stss.Items)
		}).Should(Succeed())
		Eventually(func() error {
			cms := &corev1.ConfigMapList{}
			out, err := kubectl(nil, "-n", "foo", "get", "configmap", "-o", "json")
			if err != nil {
				return err
			}
			err = json.Unmarshal(out, cms)
			if err != nil {
				return err
			}

			for _, cm := range cms.Items {
				switch cm.Name {
				case "kube-root-ca.crt", "mycnf":
				default:
					return fmt.Errorf("pending config map %+v", cm)
				}
			}
			return nil
		}).Should(Succeed())
		Eventually(func() error {
			secrets := &corev1.SecretList{}
			out, err := kubectl(nil, "-n", "foo", "get", "secret", "-o", "json")
			if err != nil {
				return err
			}
			err = json.Unmarshal(out, secrets)
			if err != nil {
				return err
			}
			if len(secrets.Items) == 1 {
				return nil
			}
			return fmt.Errorf("pending secrets: %+v", secrets.Items)
		}).Should(Succeed())
		Eventually(func() error {
			sas := &corev1.ServiceAccountList{}
			out, err := kubectl(nil, "-n", "foo", "get", "serviceaccount", "-o", "json")
			if err != nil {
				return err
			}
			err = json.Unmarshal(out, sas)
			if err != nil {
				return err
			}
			if len(sas.Items) == 1 {
				return nil
			}
			return fmt.Errorf("pending service accounts: %+v", sas.Items)
		}).Should(Succeed())
	})
})
