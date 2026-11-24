// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// certManagerGroup is the API group for cert-manager issuer references.
const certManagerGroup = "cert-manager.io"

// issuerKind is the default cert-manager issuer kind.
const issuerKind = "Issuer"

// caDuration is the lifetime of the provider-managed CA. Clients keep the CA
// from the connection details, so it must outlive many server certificates.
const caDuration = 10 * 365 * 24 * time.Hour

// tlsPath is the field path of the TLS parameters.
var tlsPath = field.NewPath("spec", "parameters", "tls")

// instanceTLS decodes the TLS parameters of in, with the defaults applied:
// disabled, and an Issuer of cert-manager.io.
func instanceTLS(in *corev1alpha1.Instance) (definition.TLSParameters, error) {
	var params definition.Parameters
	if raw := in.Spec.Parameters; raw != nil && len(raw.Raw) > 0 {
		if err := json.Unmarshal(raw.Raw, &params); err != nil {
			return definition.TLSParameters{}, fmt.Errorf("decode spec.parameters: %w", err)
		}
	}
	var tls definition.TLSParameters
	if params.TLS != nil {
		tls = *params.TLS
	}
	if tls.Mode == "" {
		tls.Mode = definition.TLSModeDisabled
	}
	if tls.Issuer != nil {
		issuer := *tls.Issuer
		if issuer.Kind == "" {
			issuer.Kind = definition.IssuerKindIssuer
		}
		if issuer.Group == "" {
			issuer.Group = certManagerGroup
		}
		tls.Issuer = &issuer
	}
	return tls, nil
}

// tlsParameters returns the Instance's TLS parameters. The runtime has
// checked them against their schema, and Validate reports what does not
// decode.
func tlsParameters(c *controller.Context) definition.TLSParameters {
	tls, err := instanceTLS(c.Instance())
	if err != nil {
		return definition.TLSParameters{Mode: definition.TLSModeDisabled}
	}
	return tls
}

// tlsMode returns the TLS mode the Instance asks for.
func tlsMode(c *controller.Context) definition.TLSMode {
	return tlsParameters(c).Mode
}

// tlsEnabled reports whether TLS is on.
func tlsEnabled(c *controller.Context) bool {
	return tlsMode(c) != definition.TLSModeDisabled
}

// tlsSecretName is the Secret ClickHouse and Keeper mount the server
// certificate from: the user's, or the one cert-manager writes.
func tlsSecretName(c *controller.Context) string {
	if name := tlsParameters(c).CertificateSecretName; name != "" {
		return name
	}
	return serverSecretName(c.Name())
}

// ensureTLS makes the server certificate available and returns a WaitError
// until its Secret holds one.
//
// With certificateSecretName the Secret is the user's. With an issuer only
// the leaf certificate is created; otherwise the provider manages a
// self-signed CA chain scoped to the Instance (self-signed Issuer → CA
// Certificate → CA Issuer → server Certificate).
func ensureTLS(c *controller.Context) error {
	tls := tlsParameters(c)
	wait := "waiting for cert-manager to issue the server certificate"
	if tls.CertificateSecretName != "" {
		wait = fmt.Sprintf("waiting for the Secret %s of %s", tls.CertificateSecretName, tlsPath.Child("certificateSecretName"))
		// Certificates of a source chosen before the cluster was created.
		if err := deleteManagedCertificates(c); err != nil {
			return err
		}
	} else {
		issuerRef, err := ensureIssuer(c, tls.Issuer)
		if err != nil {
			return err
		}
		if err := ensureServerCertificate(c, issuerRef, tls.AdditionalNames); err != nil {
			return err
		}
	}

	// Only the first issuance blocks: a re-issue (renewal, new names) leaves
	// the current certificate in place until the new one is written, and
	// ClickHouse reloads the renewed files without a restart.
	issued, err := serverCertificateIssued(c)
	if err != nil {
		return err
	}
	if !issued {
		return controller.WaitForDuration(wait, 15*time.Second)
	}
	return nil
}

// serverCertificateIssued reports whether the server certificate Secret holds
// a certificate.
func serverCertificateIssued(c *controller.Context) (bool, error) {
	secret := &corev1.Secret{}
	found, err := getOptional(c, secret, tlsSecretName(c))
	if err != nil {
		return false, err
	}
	return found && len(secret.Data[corev1.TLSCertKey]) > 0, nil
}

// ensureIssuer resolves the issuer that signs the server certificate. If the
// user provided an override it is used directly; otherwise a provider-managed
// self-signed CA chain is created and the CA Issuer reference is returned.
func ensureIssuer(c *controller.Context, override *definition.IssuerReference) (cmmeta.IssuerReference, error) {
	if override != nil {
		// A provider-managed chain of before the issuer was chosen is unused.
		if err := deleteCAChain(c); err != nil {
			return cmmeta.IssuerReference{}, err
		}
		return issuerReference(override), nil
	}

	selfSigned := &cmapi.Issuer{
		ObjectMeta: c.ObjectMeta(selfSignedIssuerName(c.Name())),
		Spec: cmapi.IssuerSpec{
			IssuerConfig: cmapi.IssuerConfig{SelfSigned: &cmapi.SelfSignedIssuer{}},
		},
	}
	if err := c.Apply(selfSigned); err != nil {
		return cmmeta.IssuerReference{}, fmt.Errorf("apply self-signed issuer: %w", err)
	}

	caCert := &cmapi.Certificate{
		ObjectMeta: c.ObjectMeta(caCertName(c.Name())),
		Spec: cmapi.CertificateSpec{
			IsCA:       true,
			Duration:   &metav1.Duration{Duration: caDuration},
			CommonName: caCertName(c.Name()),
			SecretName: caSecretName(c.Name()),
			IssuerRef: cmmeta.IssuerReference{
				Name:  selfSignedIssuerName(c.Name()),
				Kind:  issuerKind,
				Group: certManagerGroup,
			},
		},
	}
	if err := c.Apply(caCert); err != nil {
		return cmmeta.IssuerReference{}, fmt.Errorf("apply CA certificate: %w", err)
	}

	caIssuer := &cmapi.Issuer{
		ObjectMeta: c.ObjectMeta(caIssuerName(c.Name())),
		Spec: cmapi.IssuerSpec{
			IssuerConfig: cmapi.IssuerConfig{CA: &cmapi.CAIssuer{SecretName: caSecretName(c.Name())}},
		},
	}
	if err := c.Apply(caIssuer); err != nil {
		return cmmeta.IssuerReference{}, fmt.Errorf("apply CA issuer: %w", err)
	}

	return managedIssuerReference(c.Name()), nil
}

// issuerReference is the cert-manager reference of the issuer the TLS
// parameters name.
func issuerReference(issuer *definition.IssuerReference) cmmeta.IssuerReference {
	return cmmeta.IssuerReference{Name: issuer.Name, Kind: string(issuer.Kind), Group: issuer.Group}
}

// managedIssuerReference refers to the CA Issuer the provider creates.
func managedIssuerReference(instanceName string) cmmeta.IssuerReference {
	return cmmeta.IssuerReference{Name: caIssuerName(instanceName), Kind: issuerKind, Group: certManagerGroup}
}

// ensureServerCertificate creates the leaf server Certificate signed by the
// resolved issuer. It covers the client Service, every host's Service (the
// hosts' names in remote_servers), the LoadBalancer address, if any, and the
// additional names of the TLS parameters.
func ensureServerCertificate(c *controller.Context, issuerRef cmmeta.IssuerReference, additionalNames []string) error {
	l := layout(c)
	dnsNames := serverDNSNames(c.Name(), c.Namespace(), l)
	if l.keeper {
		dnsNames = append(dnsNames, keeperDNSNames(c.Name(), c.Namespace())...)
	}
	externalNames, externalIPs, err := externalAddresses(c)
	if err != nil {
		return err
	}
	cert := &cmapi.Certificate{
		ObjectMeta: c.ObjectMeta(serverCertName(c.Name())),
		Spec: cmapi.CertificateSpec{
			SecretName:  serverSecretName(c.Name()),
			CommonName:  dnsNames[0],
			DNSNames:    uniqueNames(dnsNames, externalNames, additionalNames),
			IPAddresses: externalIPs,
			IssuerRef:   issuerRef,
		},
	}
	if err := c.Apply(cert); err != nil {
		return fmt.Errorf("apply server certificate: %w", err)
	}
	return nil
}

// uniqueNames joins the lists of names, each name once, in order.
func uniqueNames(lists ...[]string) []string {
	var names []string
	for _, name := range slices.Concat(lists...) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// serverDNSNames returns the DNS names the server certificate must cover: the
// client Service and each host's Service, in every in-cluster form, and each
// host's pod names, which replicas fetch parts from each other by.
func serverDNSNames(instanceName, namespace string, l clusterLayout) []string {
	replicaServices := hostServiceNames(instanceName, l)
	names := serviceDNSNames(append([]string{clientServiceName(instanceName)}, replicaServices...), namespace)
	for _, svc := range replicaServices {
		names = append(names, fmt.Sprintf("*.%s.%s.svc.cluster.local", svc, namespace))
	}
	return names
}

// keeperDNSNames returns the names Keeper is reached by: its Service, each
// node's Service (Raft) and client Service.
func keeperDNSNames(instanceName, namespace string) []string {
	services := append([]string{"keeper-" + trimCRName(keeperCRName(instanceName))}, keeperHostNames(instanceName)...)
	return serviceDNSNames(append(services, keeperClientServiceNames(instanceName)...), namespace)
}

// serviceDNSNames returns every in-cluster form of the Services' names.
func serviceDNSNames(services []string, namespace string) []string {
	names := make([]string, 0, 4*len(services))
	for _, svc := range services {
		names = append(names,
			svc,
			fmt.Sprintf("%s.%s", svc, namespace),
			fmt.Sprintf("%s.%s.svc", svc, namespace),
			fmt.Sprintf("%s.%s.svc.cluster.local", svc, namespace),
		)
	}
	return names
}

// clientServiceName is the operator's Service for the whole installation.
func clientServiceName(instanceName string) string {
	return "clickhouse-" + instanceName
}

// hostServiceNames returns the operator's per-host Service names,
// chi-{chi}-{cluster}-{shard}-{replica}, which are also the hosts' names in
// remote_servers. The operator cuts the CHI name to 60 characters and trims
// separators from the ends.
func hostServiceNames(instanceName string, l clusterLayout) []string {
	chiName := trimCRName(instanceName)
	names := make([]string, 0, l.hosts())
	for shard := 0; shard < l.shards; shard++ {
		for replica := 0; replica < l.replicas; replica++ {
			names = append(names, fmt.Sprintf("chi-%s-%s-%d-%d", chiName, common.CHIClusterName, shard, replica))
		}
	}
	return names
}

// readCACert returns the CA that signed the server certificate, from the
// Secret that holds it. Issuers such as ACME do not provide one.
func readCACert(c *controller.Context) string {
	secret := &corev1.Secret{}
	if err := c.Get(secret, tlsSecretName(c)); err != nil {
		return ""
	}
	return string(secret.Data[cmmeta.TLSCAKey])
}

// instanceForClientService maps an operator client Service to the Instance
// whose CHI it serves (the CHI and the Instance share the name).
func instanceForClientService(_ context.Context, obj client.Object) []reconcile.Request {
	chiName := obj.GetLabels()[common.LabelCHIName]
	if chiName == "" || obj.GetLabels()[labelOperatorService] != operatorServiceCR || obj.GetName() != clientServiceName(chiName) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: chiName}}}
}

// loadBalancerAddressChanged passes LoadBalancer Services whose address was
// assigned or changed.
func loadBalancerAddressChanged() predicate.Predicate {
	isLoadBalancer := func(obj client.Object) bool {
		svc, ok := obj.(*corev1.Service)
		return ok && svc.Spec.Type == corev1.ServiceTypeLoadBalancer
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isLoadBalancer(e.Object) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldSvc, okOld := e.ObjectOld.(*corev1.Service)
			newSvc, okNew := e.ObjectNew.(*corev1.Service)
			return okOld && okNew && isLoadBalancer(newSvc) &&
				!equality.Semantic.DeepEqual(oldSvc.Status.LoadBalancer, newSvc.Status.LoadBalancer)
		},
	}
}

// The operator labels the Service of the whole installation with
// clickhouse.altinity.com/Service=chi.
const (
	labelOperatorService = "clickhouse.altinity.com/Service"
	operatorServiceCR    = "chi"
)

// trimCRName shortens a CHI or CHK name as the operator does in the names
// of the objects it creates: cut to 60 characters, separators trimmed.
func trimCRName(name string) string {
	if len(name) > 60 {
		name = name[:60]
	}
	return strings.Trim(name, "-_.")
}

// externalAddresses returns the LoadBalancer hostnames and IPs of the client
// Service, so clients outside the cluster can verify the certificate too.
func externalAddresses(c *controller.Context) (names, ips []string, err error) {
	svc := &corev1.Service{}
	found, err := getOptional(c, svc, clientServiceName(c.Name()))
	if err != nil || !found || svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return nil, nil, err
	}
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.Hostname != "" {
			names = append(names, ingress.Hostname)
		}
		if ingress.IP != "" {
			ips = append(ips, ingress.IP)
		}
	}
	return names, ips, nil
}

// applyTLSToCHISpec wires the server certificate and secure ports into the CHI
// configuration, and makes distributed queries and replication between
// replicas use TLS. The plaintext ports stay open: the operator talks to
// ClickHouse over plain HTTP (see plaintextPolicy for who may reach them).
func applyTLSToCHISpec(spec *chiv1.ChiSpec, settings *chiv1.Settings, cluster *chiv1.Cluster, secretName string) {
	spec.Configuration.Files = buildTLSFiles(secretName)
	settings.Set("https_port", chiv1.NewSettingScalar(strconv.Itoa(common.HTTPSPort)))
	settings.Set("tcp_port_secure", chiv1.NewSettingScalar(strconv.Itoa(common.TCPSecurePort)))
	cluster.Secure = chtypes.NewStringBool(true)
	// Replicas fetch parts over HTTPS. ClickHouse refuses both interserver
	// ports, and its default configuration sets the plaintext one; the
	// operator renders "_removed_" as a removal. During a rolling switch
	// replicas fetch from each other over whichever one the source serves.
	settings.Set("interserver_https_port", chiv1.NewSettingScalar(strconv.Itoa(common.InterserverHTTPSPort)))
	settings.Set("interserver_http_port", chiv1.NewSettingScalar("_removed_"))
}

// buildTLSFiles renders the ClickHouse config files that enable TLS: an
// openssl.xml pointing at the mounted certificate files plus the certificate,
// key and CA sourced from the certificate Secret, whose keys
// (tls.crt/tls.key/ca.crt) are mapped onto ClickHouse's expected file names.
func buildTLSFiles(secretName string) *chiv1.Settings {
	files := chiv1.NewSettings()
	files.Set("openssl.xml", chiv1.NewSettingScalar(opensslXML(secretName)))
	files.Set("server.crt", secretFileSetting(secretName, "tls.crt"))
	files.Set("server.key", secretFileSetting(secretName, "tls.key"))
	files.Set("ca.crt", secretFileSetting(secretName, "ca.crt"))
	return files
}

// secretFileSetting builds a config file whose contents are sourced from a
// Secret key. The operator mounts it at
// /etc/clickhouse-server/secrets.d/<file>/<secret>/<key>.
func secretFileSetting(secretName, key string) *chiv1.Setting {
	return chiv1.NewSettingSource(&chiv1.SettingSource{
		ValueFrom: &chtypes.DataSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	})
}

// opensslXML renders the ClickHouse openSSL configuration referencing the
// certificate files mounted from the cert-manager secret. Server-auth TLS only:
// clients validate the server certificate; client certificates are not required.
// The client side is what a replica uses for distributed queries to the other
// replicas: it trusts the issuing CA as well as the system CAs.
func opensslXML(secretName string) string {
	base := "/etc/clickhouse-server/secrets.d"
	return fmt.Sprintf(`<clickhouse>
    <openSSL>
        <server>
            <certificateFile>%[1]s/server.crt/%[2]s/tls.crt</certificateFile>
            <privateKeyFile>%[1]s/server.key/%[2]s/tls.key</privateKeyFile>
            <caConfig>%[1]s/ca.crt/%[2]s/ca.crt</caConfig>
            <verificationMode>none</verificationMode>
            <cacheSessions>true</cacheSessions>
            <disableProtocols>sslv2,sslv3</disableProtocols>
            <preferServerCiphers>true</preferServerCiphers>
        </server>
        <client>
            <caConfig>%[1]s/ca.crt/%[2]s/ca.crt</caConfig>
            <loadDefaultCAFile>true</loadDefaultCAFile>
            <verificationMode>relaxed</verificationMode>
            <cacheSessions>true</cacheSessions>
            <disableProtocols>sslv2,sslv3</disableProtocols>
            <preferServerCiphers>true</preferServerCiphers>
            <invalidCertificateHandler>
                <name>RejectCertificateHandler</name>
            </invalidCertificateHandler>
        </client>
    </openSSL>
</clickhouse>
`, base, secretName)
}

// deleteTLSResources removes the provider-managed cert-manager resources and the
// issued certificate secrets. cert-manager does not set an owner reference on
// the secrets by default, so they are deleted explicitly. Without cert-manager
// installed there is nothing to delete.
func deleteTLSResources(c *controller.Context) error {
	return deleteAll(c,
		&cmapi.Certificate{ObjectMeta: c.ObjectMeta(serverCertName(c.Name()))},
		&corev1.Secret{ObjectMeta: c.ObjectMeta(serverSecretName(c.Name()))},
		&cmapi.Certificate{ObjectMeta: c.ObjectMeta(caCertName(c.Name()))},
		&cmapi.Issuer{ObjectMeta: c.ObjectMeta(caIssuerName(c.Name()))},
		&cmapi.Issuer{ObjectMeta: c.ObjectMeta(selfSignedIssuerName(c.Name()))},
		&corev1.Secret{ObjectMeta: c.ObjectMeta(caSecretName(c.Name()))},
	)
}

// deleteCAChain removes the provider-managed CA chain, if it exists.
func deleteCAChain(c *controller.Context) error {
	found, err := getOptional(c, &corev1.Secret{}, caSecretName(c.Name()))
	if err != nil || !found {
		return err
	}
	return deleteAll(c,
		&cmapi.Certificate{ObjectMeta: c.ObjectMeta(caCertName(c.Name()))},
		&cmapi.Issuer{ObjectMeta: c.ObjectMeta(caIssuerName(c.Name()))},
		&cmapi.Issuer{ObjectMeta: c.ObjectMeta(selfSignedIssuerName(c.Name()))},
		&corev1.Secret{ObjectMeta: c.ObjectMeta(caSecretName(c.Name()))},
	)
}

// cleanupTLS removes the certificates of an Instance whose TLS was turned
// off, once no ClickHouse pod mounts them: right after the CHI is applied its
// status still reads Completed while the pods have yet to roll, and a pod
// restarting before its turn would wait for the deleted Secret.
func cleanupTLS(c *controller.Context, chi *chiv1.ClickHouseInstallation) error {
	if chi.Spec.Configuration != nil && chi.Spec.Configuration.Files != nil &&
		chi.Spec.Configuration.Files.Has("server.crt") {
		return nil
	}
	chk := &chkv1.ClickHouseKeeperInstallation{}
	found, err := getOptional(c, chk, keeperCRName(c.Name()))
	if err != nil {
		return err
	}
	if found && chk.Spec.Configuration != nil && chk.Spec.Configuration.Files != nil &&
		chk.Spec.Configuration.Files.Has("server.crt") {
		return nil
	}
	if mounted, err := serverSecretMounted(c); err != nil || mounted {
		return err
	}
	return deleteManagedCertificates(c)
}

// deleteManagedCertificates removes the certificates the provider requested
// from cert-manager, if there are any. The Secret of certificateSecretName
// is the user's and stays.
func deleteManagedCertificates(c *controller.Context) error {
	found, err := getOptional(c, &corev1.Secret{}, serverSecretName(c.Name()))
	if err != nil {
		return err
	}
	caFound, err := getOptional(c, &corev1.Secret{}, caSecretName(c.Name()))
	if err != nil || (!found && !caFound) {
		return err
	}
	log.FromContext(c.Context()).Info("Deleting the unused cert-manager certificates")
	return deleteTLSResources(c)
}

// serverSecretMounted reports whether a ClickHouse or Keeper pod mounts the
// server certificate Secret.
func serverSecretMounted(c *controller.Context) (bool, error) {
	secretName := serverSecretName(c.Name())
	for _, selector := range []client.MatchingLabels{
		{common.LabelCHIName: c.Name()},
		{common.LabelCHKName: keeperCRName(c.Name())},
	} {
		pods := &corev1.PodList{}
		if err := c.List(pods, selector); err != nil {
			return false, fmt.Errorf("list pods: %w", err)
		}
		for _, pod := range pods.Items {
			for _, volume := range pod.Spec.Volumes {
				if volume.Secret != nil && volume.Secret.SecretName == secretName {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func deleteAll(c *controller.Context, objects ...client.Object) error {
	for _, obj := range objects {
		if err := c.Delete(obj); err != nil && !meta.IsNoMatchError(err) {
			return fmt.Errorf("delete %T %s: %w", obj, obj.GetName(), err)
		}
	}
	return nil
}

func selfSignedIssuerName(instanceName string) string {
	return instanceName + common.SelfSignedIssuerSuffix
}
func caCertName(instanceName string) string     { return instanceName + common.CAIssuerSuffix }
func caIssuerName(instanceName string) string   { return instanceName + common.CAIssuerSuffix }
func caSecretName(instanceName string) string   { return instanceName + common.CACertSecretSuffix }
func serverCertName(instanceName string) string { return instanceName + common.ServerCertSuffix }
func serverSecretName(instanceName string) string {
	return instanceName + common.ServerCertSecretSuffix
}
