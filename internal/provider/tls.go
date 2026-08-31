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
	"fmt"
	"strconv"
	"time"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chtypes "github.com/altinity/clickhouse-operator/pkg/apis/common/types"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition/components"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// certManagerGroup is the API group for cert-manager issuer references.
const certManagerGroup = "cert-manager.io"

// issuerKind is the default cert-manager issuer kind.
const issuerKind = "Issuer"

// tlsEnabled reports whether TLS is requested for the engine component.
func tlsEnabled(c *controller.Context) bool {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	var params components.ClickHouseParameters
	if !c.TryDecodeComponentParameters(engine, &params) {
		return false
	}
	return params.TLS != nil && params.TLS.Enabled
}

// ensureTLS provisions the cert-manager resources required for TLS and returns a
// WaitError until the server certificate has been issued.
//
// When the user supplies an issuerRef override only the leaf certificate is
// created; otherwise the provider manages a self-signed CA chain scoped to the
// Instance (self-signed Issuer → CA Certificate → CA Issuer → server Certificate).
func ensureTLS(c *controller.Context) error {
	engine := c.Instance().Spec.Components[common.ComponentEngine]
	var params components.ClickHouseParameters
	_ = c.TryDecodeComponentParameters(engine, &params)

	var override *components.IssuerRef
	if params.TLS != nil {
		override = params.TLS.IssuerRef
	}

	issuerRef, err := ensureIssuer(c, override)
	if err != nil {
		return err
	}

	if err := ensureServerCertificate(c, issuerRef); err != nil {
		return err
	}

	cert := &cmapi.Certificate{}
	if err := c.Get(cert, serverCertName(c.Name())); err != nil {
		return controller.WaitForDuration("waiting for server certificate to be created", 15*time.Second)
	}
	if !certificateReady(cert) {
		return controller.WaitForDuration("waiting for cert-manager to issue server certificate", 15*time.Second)
	}

	log.FromContext(c.Context()).Info("Server certificate is ready", "name", serverCertName(c.Name()))
	return nil
}

// ensureIssuer resolves the issuer that signs the server certificate. If the
// user provided an override it is used directly; otherwise a provider-managed
// self-signed CA chain is created and the CA Issuer reference is returned.
func ensureIssuer(c *controller.Context, override *components.IssuerRef) (cmmeta.IssuerReference, error) {
	if override != nil && override.Name != "" {
		kind := override.Kind
		if kind == "" {
			kind = issuerKind
		}
		group := override.Group
		if group == "" {
			group = certManagerGroup
		}
		return cmmeta.IssuerReference{Name: override.Name, Kind: kind, Group: group}, nil
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

	return cmmeta.IssuerReference{Name: caIssuerName(c.Name()), Kind: issuerKind, Group: certManagerGroup}, nil
}

// ensureServerCertificate creates the leaf server Certificate signed by the
// resolved issuer, covering the root Service DNS names.
func ensureServerCertificate(c *controller.Context, issuerRef cmmeta.IssuerReference) error {
	dnsNames := serverDNSNames(c.Name(), c.Namespace())
	cert := &cmapi.Certificate{
		ObjectMeta: c.ObjectMeta(serverCertName(c.Name())),
		Spec: cmapi.CertificateSpec{
			SecretName: serverSecretName(c.Name()),
			CommonName: dnsNames[0],
			DNSNames:   dnsNames,
			IssuerRef:  issuerRef,
		},
	}
	if err := c.Apply(cert); err != nil {
		return fmt.Errorf("apply server certificate: %w", err)
	}
	return nil
}

// certificateReady reports whether a cert-manager Certificate has been issued.
func certificateReady(cert *cmapi.Certificate) bool {
	for _, cond := range cert.Status.Conditions {
		if cond.Type == cmapi.CertificateConditionReady && cond.Status == cmmeta.ConditionTrue {
			return true
		}
	}
	return false
}

// serverDNSNames returns the DNS names the server certificate must cover. The
// Altinity operator exposes the root Service as clickhouse-<name>.
func serverDNSNames(instanceName, namespace string) []string {
	svc := fmt.Sprintf("clickhouse-%s", instanceName)
	return []string{
		svc,
		fmt.Sprintf("%s.%s", svc, namespace),
		fmt.Sprintf("%s.%s.svc", svc, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", svc, namespace),
	}
}

// applyTLSToCHISpec wires the server certificate and secure ports into the CHI
// configuration. The secure ports are added alongside the plaintext ports so
// existing clients keep working.
func applyTLSToCHISpec(spec *chiv1.ChiSpec, settings *chiv1.Settings, instanceName string) {
	spec.Configuration.Files = buildTLSFiles(serverSecretName(instanceName))
	settings.Set("https_port", chiv1.NewSettingScalar(strconv.Itoa(common.HTTPSPort)))
	settings.Set("tcp_port_secure", chiv1.NewSettingScalar(strconv.Itoa(common.TCPSecurePort)))
}

// buildTLSFiles renders the ClickHouse config files that enable TLS: an
// openssl.xml pointing at the mounted certificate files plus the certificate,
// key and CA sourced from the cert-manager secret. cert-manager secret keys
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

// opensslXML renders the ClickHouse openSSL server configuration referencing the
// certificate files mounted from the cert-manager secret. Server-auth TLS only:
// clients validate the server certificate; client certificates are not required.
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
    </openSSL>
</clickhouse>
`, base, secretName)
}

// deleteTLSResources removes the provider-managed cert-manager resources and the
// issued certificate secrets. cert-manager does not set an owner reference on
// the secrets by default, so they are deleted explicitly.
func deleteTLSResources(c *controller.Context) error {
	objects := []client.Object{
		&cmapi.Certificate{ObjectMeta: c.ObjectMeta(serverCertName(c.Name()))},
		&cmapi.Certificate{ObjectMeta: c.ObjectMeta(caCertName(c.Name()))},
		&cmapi.Issuer{ObjectMeta: c.ObjectMeta(caIssuerName(c.Name()))},
		&cmapi.Issuer{ObjectMeta: c.ObjectMeta(selfSignedIssuerName(c.Name()))},
		&corev1.Secret{ObjectMeta: c.ObjectMeta(serverSecretName(c.Name()))},
		&corev1.Secret{ObjectMeta: c.ObjectMeta(caSecretName(c.Name()))},
	}
	for _, obj := range objects {
		if err := c.Delete(obj); err != nil {
			return fmt.Errorf("delete %T: %w", obj, err)
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
