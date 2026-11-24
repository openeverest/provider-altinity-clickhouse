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
	"cmp"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// The values the TLS parameters accept.
var (
	tlsModes    = []definition.TLSMode{definition.TLSModeDisabled, definition.TLSModePrefer, definition.TLSModeRequire}
	issuerKinds = []definition.IssuerKind{definition.IssuerKindIssuer, definition.IssuerKindClusterIssuer}
)

// ownSecretSuffixes name, after the Instance, the Secrets the provider, the
// runtime and cert-manager write for it.
var ownSecretSuffixes = []string{
	common.ServerCertSecretSuffix, common.CACertSecretSuffix, common.CredentialsSecretSuffix,
	common.RolloutStateSuffix, "-conn",
}

// validateTLS checks the TLS parameters of in on their own.
func validateTLS(in *corev1alpha1.Instance) field.ErrorList {
	tls, err := instanceTLS(in)
	if err != nil {
		return field.ErrorList{field.Invalid(field.NewPath("spec", "parameters"), field.OmitValueType{}, err.Error())}
	}

	var errs field.ErrorList
	if !slices.Contains(tlsModes, tls.Mode) {
		errs = append(errs, field.NotSupported(tlsPath.Child("mode"), tls.Mode, tlsModes))
	}
	if tls.Mode == definition.TLSModeDisabled &&
		(tls.CertificateSecretName != "" || tls.Issuer != nil || len(tls.AdditionalNames) > 0) {
		errs = append(errs, field.Forbidden(tlsPath,
			"certificateSecretName, issuer and additionalNames apply only with TLS on: set mode to preferTLS or requireTLS"))
	}

	if name := tls.CertificateSecretName; name != "" {
		path := tlsPath.Child("certificateSecretName")
		if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
			errs = append(errs, field.Invalid(path, name, strings.Join(msgs, "; ")))
		}
		for _, suffix := range ownSecretSuffixes {
			if name == in.Name+suffix {
				errs = append(errs, field.Invalid(path, name,
					"the name of a Secret created for the instance, which would overwrite yours; name the Secret differently"))
			}
		}
	}

	if issuer := tls.Issuer; issuer != nil {
		path := tlsPath.Child("issuer")
		if tls.CertificateSecretName != "" {
			errs = append(errs, field.Forbidden(path, "use either certificateSecretName or an issuer, not both"))
		}
		if issuer.Name == "" {
			errs = append(errs, field.Required(path.Child("name"), ""))
		} else if msgs := validation.IsDNS1123Subdomain(issuer.Name); len(msgs) > 0 {
			errs = append(errs, field.Invalid(path.Child("name"), issuer.Name, strings.Join(msgs, "; ")))
		}
		if issuer.Group == certManagerGroup && !slices.Contains(issuerKinds, issuer.Kind) {
			errs = append(errs, field.NotSupported(path.Child("kind"), issuer.Kind, issuerKinds))
		}
	}

	namesPath := tlsPath.Child("additionalNames")
	if len(tls.AdditionalNames) > 0 && tls.CertificateSecretName != "" {
		errs = append(errs, field.Forbidden(namesPath,
			"cannot be combined with certificateSecretName: the certificate in the Secret carries its own names"))
	}
	for i, name := range tls.AdditionalNames {
		if net.ParseIP(name) != nil {
			errs = append(errs, field.Invalid(namesPath.Index(i), name, "must be a DNS name, not an IP address"))
			continue
		}
		if msgs := validation.IsDNS1123Subdomain(strings.TrimPrefix(name, "*.")); len(msgs) > 0 {
			errs = append(errs, field.Invalid(namesPath.Index(i), name, "must be a DNS name: "+strings.Join(msgs, "; ")))
		}
	}
	return errs
}

// certificateSource identifies where an instance's certificate comes from.
func certificateSource(tls definition.TLSParameters) string {
	switch {
	case tls.CertificateSecretName != "":
		return "the Secret " + tls.CertificateSecretName
	case tls.Issuer != nil:
		return describeIssuer(issuerReference(tls.Issuer))
	}
	return "the CA created for the instance"
}

func describeIssuer(ref cmmeta.IssuerReference) string {
	return fmt.Sprintf("the %s %s of %s", cmp.Or(ref.Kind, issuerKind), ref.Name, cmp.Or(ref.Group, certManagerGroup))
}

const (
	tlsOffForbidden    = "TLS cannot be turned off on an existing instance: disabled can only be set when the instance is created"
	tlsSourceForbidden = "certificateSecretName and issuer cannot change while TLS is on: "
)

// validateTLSTransition rejects, on an update of an Instance whose cluster
// the provider has started creating, turning TLS off and changing where the
// certificate comes from. validateDeployedTLS checks the same at reconcile
// time, against the running cluster.
func validateTLSTransition(in, old *corev1alpha1.Instance) field.ErrorList {
	before, err := instanceTLS(old)
	if err != nil || before.Mode == definition.TLSModeDisabled {
		return nil
	}
	after, err := instanceTLS(in)
	if err != nil {
		return nil
	}
	if after.Mode == definition.TLSModeDisabled {
		return field.ErrorList{field.Forbidden(tlsPath.Child("mode"), tlsOffForbidden)}
	}
	if from, to := certificateSource(before), certificateSource(after); from != to {
		return field.ErrorList{field.Forbidden(tlsPath, tlsSourceForbidden+"the certificate comes from "+from)}
	}
	return nil
}

// validateDeployedTLS is validateTLSTransition against the running
// ClickHouseInstallation and the server Certificate: the previous Instance is
// not always at hand, and one of an earlier release carried TLS in the engine
// parameters. It also checks the certificate of certificateSecretName, once
// the Secret exists: Sync waits for a missing one.
func validateDeployedTLS(c *controller.Context) (field.ErrorList, error) {
	desired := tlsParameters(c)
	var errs field.ErrorList
	if desired.CertificateSecretName != "" {
		secret := &corev1.Secret{}
		found, err := getOptional(c, secret, desired.CertificateSecretName)
		if err != nil {
			return nil, err
		}
		if found {
			if err := checkCertificateSecret(secret, internalTLSNames(c)); err != nil {
				errs = append(errs, field.Invalid(tlsPath.Child("certificateSecretName"), secret.Name, err.Error()))
			}
		}
	}

	chi := &chiv1.ClickHouseInstallation{}
	found, err := getOptional(c, chi, c.Name())
	if err != nil || !found {
		return errs, err
	}
	applied := appliedTLSSecret(chi)
	if applied == "" {
		return errs, nil
	}
	if desired.Mode == definition.TLSModeDisabled {
		return append(errs, field.Forbidden(tlsPath.Child("mode"), tlsOffForbidden)), nil
	}
	if applied != tlsSecretName(c) {
		from := "the Secret " + applied
		if applied == serverSecretName(c.Name()) {
			from = "cert-manager"
		}
		return append(errs, field.Forbidden(tlsPath, tlsSourceForbidden+"the instance serves the certificate of "+from)), nil
	}
	if desired.CertificateSecretName != "" {
		return errs, nil
	}

	want := managedIssuerReference(c.Name())
	if desired.Issuer != nil {
		want = issuerReference(desired.Issuer)
	}
	current, found, err := serverCertificateIssuer(c)
	if err != nil || !found {
		return errs, err
	}
	if current.Name != want.Name || cmp.Or(current.Kind, issuerKind) != want.Kind || cmp.Or(current.Group, certManagerGroup) != want.Group {
		from := describeIssuer(current)
		if current == managedIssuerReference(c.Name()) {
			from = "the CA created for the instance"
		}
		errs = append(errs, field.Forbidden(tlsPath.Child("issuer"),
			tlsSourceForbidden+"the server certificate is issued by "+from))
	}
	return errs, nil
}

// appliedTLSSecret names the Secret chi serves its certificate from, or ""
// when it was applied without TLS.
func appliedTLSSecret(chi *chiv1.ClickHouseInstallation) string {
	cfg := chi.Spec.Configuration
	if cfg == nil || cfg.Settings == nil || !cfg.Settings.Has("https_port") ||
		cfg.Files == nil || !cfg.Files.Has("server.crt") {
		return ""
	}
	if ref := cfg.Files.Get("server.crt").GetSecretKeyRef(); ref != nil {
		return ref.Name
	}
	return ""
}

// serverCertificateIssuer reads the issuer of the server Certificate from
// the API server: the client caches no Certificates, which a cluster without
// cert-manager does not even serve.
func serverCertificateIssuer(c *controller.Context) (cmmeta.IssuerReference, bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(cmapi.SchemeGroupVersion.WithKind(cmapi.CertificateKind))
	if err := c.Get(obj, serverCertName(c.Name())); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return cmmeta.IssuerReference{}, false, nil
		}
		return cmmeta.IssuerReference{}, false, fmt.Errorf("get the server Certificate: %w", err)
	}
	certificate := &cmapi.Certificate{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, certificate); err != nil {
		return cmmeta.IssuerReference{}, false, fmt.Errorf("decode the server Certificate: %w", err)
	}
	return certificate.Spec.IssuerRef, true, nil
}

// internalTLSNames are the names ClickHouse and Keeper verify each other's
// certificate against, and the in-cluster name clients connect to. A
// certificate of certificateSecretName has to be valid for all of them.
func internalTLSNames(c *controller.Context) []string {
	names := []string{fmt.Sprintf("%s.%s.svc.cluster.local", clientServiceName(c.Name()), c.Namespace())}
	l := layout(c)
	if !l.keeper {
		return names
	}
	// Distributed queries address the replicas by their Services' short
	// names, replication fetches by their pods' names.
	for _, host := range hostServiceNames(c.Name(), l) {
		names = append(names, host, fmt.Sprintf("%[1]s-0.%[1]s.%[2]s.svc.cluster.local", host, c.Namespace()))
	}
	// The Keeper nodes address each other (Raft) by short name too;
	// ClickHouse reaches them through their client Services.
	names = append(names, keeperHostNames(c.Name())...)
	for _, svc := range keeperClientServiceNames(c.Name()) {
		names = append(names, fmt.Sprintf("%s.%s.svc.cluster.local", svc, c.Namespace()))
	}
	return names
}

// checkCertificateSecret checks a certificate Secret the user provides: it
// holds a certificate and its key (tls.crt, tls.key) and the CA certificate
// (ca.crt), the CA verifies the certificate, as the replicas and clients
// will, and the certificate is valid for names.
func checkCertificateSecret(secret *corev1.Secret, names []string) error {
	var missing []string
	for _, key := range []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey, cmmeta.TLSCAKey} {
		if len(secret.Data[key]) == 0 {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the Secret has no %s", strings.Join(missing, ", "))
	}
	if _, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey]); err != nil {
		return fmt.Errorf("tls.crt and tls.key do not hold a certificate and its private key: %w", err)
	}
	chain, err := parseCertificates(secret.Data[corev1.TLSCertKey])
	if err != nil {
		return fmt.Errorf("tls.crt: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data[cmmeta.TLSCAKey]) {
		return errors.New("ca.crt holds no PEM certificate")
	}
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}
	leaf := chain[0]
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("the CA of ca.crt does not verify the certificate of tls.crt for server authentication: %w", err)
	}
	var uncovered []string
	for _, name := range names {
		if leaf.VerifyHostname(name) != nil {
			uncovered = append(uncovered, name)
		}
	}
	if len(uncovered) > 0 {
		return fmt.Errorf("the certificate is not valid for %s: ClickHouse, Keeper and in-cluster clients connect by these names",
			strings.Join(uncovered, ", "))
	}
	return nil
}

// parseCertificates decodes the PEM certificates of data, at least one.
func parseCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("no PEM certificate")
	}
	return certs, nil
}
