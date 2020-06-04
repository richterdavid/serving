/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resources

import (
	"context"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"knative.dev/pkg/kmeta"
	"knative.dev/serving/pkg/activator"
	apisconfig "knative.dev/serving/pkg/apis/config"
	"knative.dev/serving/pkg/apis/networking"
	netv1alpha1 "knative.dev/serving/pkg/apis/networking/v1alpha1"
	"knative.dev/serving/pkg/apis/serving"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	"knative.dev/serving/pkg/network"
	"knative.dev/serving/pkg/reconciler/route/config"
	"knative.dev/serving/pkg/reconciler/route/domains"
	"knative.dev/serving/pkg/reconciler/route/resources/labels"
	"knative.dev/serving/pkg/reconciler/route/resources/names"
	"knative.dev/serving/pkg/reconciler/route/traffic"
)

// MakeIngressTLS creates IngressTLS to configure the ingress TLS.
func MakeIngressTLS(cert *netv1alpha1.Certificate, hostNames []string) netv1alpha1.IngressTLS {
	return netv1alpha1.IngressTLS{
		Hosts:           hostNames,
		SecretName:      cert.Spec.SecretName,
		SecretNamespace: cert.Namespace,
	}
}

// MakeIngress creates Ingress to set up routing rules. Such Ingress specifies
// which Hosts that it applies to, as well as the routing rules.
func MakeIngress(
	ctx context.Context,
	r *servingv1.Route,
	tc *traffic.Config,
	tls []netv1alpha1.IngressTLS,
	ingressClass string,
	defaults apisconfig.Defaults,
	acmeChallenges ...netv1alpha1.HTTP01Challenge,
) (*netv1alpha1.Ingress, error) {
	spec, err := MakeIngressSpec(ctx, r, tls, tc.Targets, tc.Visibility, defaults, acmeChallenges...)
	if err != nil {
		return nil, err
	}
	return &netv1alpha1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.Ingress(r),
			Namespace: r.Namespace,
			Labels: kmeta.UnionMaps(r.ObjectMeta.Labels, map[string]string{
				serving.RouteLabelKey:          r.Name,
				serving.RouteNamespaceLabelKey: r.Namespace,
			}),
			Annotations: kmeta.FilterMap(kmeta.UnionMaps(map[string]string{
				networking.IngressClassAnnotationKey: ingressClass,
			}, r.GetAnnotations()), func(key string) bool {
				return key == corev1.LastAppliedConfigAnnotation
			}),
			OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(r)},
		},
		Spec: spec,
	}, nil
}

// MakeIngressSpec creates a new IngressSpec
func MakeIngressSpec(
	ctx context.Context,
	r *servingv1.Route,
	tls []netv1alpha1.IngressTLS,
	targets map[string]traffic.RevisionTargets,
	visibility map[string]netv1alpha1.IngressVisibility,
	defaults apisconfig.Defaults,
	acmeChallenges ...netv1alpha1.HTTP01Challenge,
) (netv1alpha1.IngressSpec, error) {
	// Domain should have been specified in route status
	// before calling this func.
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	// Sort the names to give things a deterministic ordering.
	sort.Strings(names)
	// The routes are matching rule based on domain name to traffic split targets.
	rules := make([]netv1alpha1.IngressRule, 0, len(names))
	challengeHosts := getChallengeHosts(acmeChallenges)

	networkConfig := config.FromContext(ctx).Network

	for _, name := range names {
		visibilities := []netv1alpha1.IngressVisibility{netv1alpha1.IngressVisibilityClusterLocal}
		// If this is a public target (or not being marked as cluster-local), we also make public rule.
		if v, ok := visibility[name]; !ok || v == netv1alpha1.IngressVisibilityExternalIP {
			visibilities = append(visibilities, netv1alpha1.IngressVisibilityExternalIP)
		}
		for _, visibility := range visibilities {
			domain, err := routeDomain(ctx, name, r, visibility)
			if err != nil {
				return netv1alpha1.IngressSpec{}, err
			}
			rule := *makeIngressRule([]string{domain}, r.Namespace, visibility, targets[name], defaults)
			if networkConfig.TagHeaderBasedRouting {
				if rule.HTTP.Paths[0].AppendHeaders == nil {
					rule.HTTP.Paths[0].AppendHeaders = make(map[string]string)
				}

				if name == traffic.DefaultTarget {
					// To provide a information if a request is routed via the "default route" or not,
					// the header "Knative-Serving-Default-Route: true" is appended here.
					// If the header has "true" and there is a "Knative-Serving-Tag" header,
					// then the request is having the undefined tag header, which will be observed in queue-proxy.
					rule.HTTP.Paths[0].AppendHeaders[network.DefaultRouteHeaderName] = "true"
					// Add ingress paths for a request with the tag header.
					// If a request has one of the `names`(tag name) except the default path,
					// the request will be routed via one of the ingress paths, corresponding to the tag name.
					rule.HTTP.Paths = append(
						makeTagBasedRoutingIngressPaths(r.Namespace, targets, names, defaults), rule.HTTP.Paths...)
				} else {
					// If a request is routed by a tag-attached hostname instead of the tag header,
					// the request may not have the tag header "Knative-Serving-Tag",
					// even though the ingress path used in the case is also originated
					// from the same Knative route with the ingress path for the tag based routing.
					//
					// To prevent such inconsistency,
					// the tag header is appended with the tag corresponding to the tag-attached hostname
					rule.HTTP.Paths[0].AppendHeaders[network.TagHeaderName] = name
				}
			}
			// If this is a public rule, we need to configure ACME challenge paths.
			if visibility == netv1alpha1.IngressVisibilityExternalIP {
				rule.HTTP.Paths = append(
					makeACMEIngressPaths(challengeHosts, []string{domain}), rule.HTTP.Paths...)
			}
			rules = append(rules, rule)
		}
	}

	return netv1alpha1.IngressSpec{
		Rules: rules,
		TLS:   tls,
	}, nil
}

func getChallengeHosts(challenges []netv1alpha1.HTTP01Challenge) map[string]netv1alpha1.HTTP01Challenge {
	c := make(map[string]netv1alpha1.HTTP01Challenge, len(challenges))

	for _, challenge := range challenges {
		c[challenge.URL.Host] = challenge
	}

	return c
}

func routeDomain(ctx context.Context, targetName string, r *servingv1.Route, visibility netv1alpha1.IngressVisibility) (string, error) {
	hostname, err := domains.HostnameFromTemplate(ctx, r.Name, targetName)
	if err != nil {
		return "", err
	}

	meta := r.ObjectMeta.DeepCopy()
	isClusterLocal := visibility == netv1alpha1.IngressVisibilityClusterLocal
	labels.SetVisibility(meta, isClusterLocal)

	return domains.DomainNameFromTemplate(ctx, *meta, hostname)
}

func makeACMEIngressPaths(challenges map[string]netv1alpha1.HTTP01Challenge, domains []string) []netv1alpha1.HTTPIngressPath {
	paths := make([]netv1alpha1.HTTPIngressPath, 0, len(challenges))
	for _, domain := range domains {
		challenge, ok := challenges[domain]
		if !ok {
			continue
		}

		paths = append(paths, netv1alpha1.HTTPIngressPath{
			Splits: []netv1alpha1.IngressBackendSplit{{
				IngressBackend: netv1alpha1.IngressBackend{
					ServiceNamespace: challenge.ServiceNamespace,
					ServiceName:      challenge.ServiceName,
					ServicePort:      challenge.ServicePort,
				},
				Percent: 100,
			}},
			Path: challenge.URL.Path,
		})
	}
	return paths
}

func makeIngressRule(domains []string, ns string, visibility netv1alpha1.IngressVisibility, targets traffic.RevisionTargets, defaults apisconfig.Defaults) *netv1alpha1.IngressRule {
	return &netv1alpha1.IngressRule{
		Hosts:      domains,
		Visibility: visibility,
		HTTP: &netv1alpha1.HTTPIngressRuleValue{
			Paths: []netv1alpha1.HTTPIngressPath{
				*makeBaseIngressPath(ns, targets, defaults),
			},
		},
	}
}

func makeTagBasedRoutingIngressPaths(ns string, targets map[string]traffic.RevisionTargets, names []string, defaults apisconfig.Defaults) []netv1alpha1.HTTPIngressPath {
	paths := make([]netv1alpha1.HTTPIngressPath, 0, len(names))

	for _, name := range names {
		if name != traffic.DefaultTarget {
			path := makeBaseIngressPath(ns, targets[name], defaults)
			path.Headers = map[string]netv1alpha1.HeaderMatch{network.TagHeaderName: {Exact: name}}
			paths = append(paths, *path)
		}
	}

	return paths
}

func makeBaseIngressPath(ns string, targets traffic.RevisionTargets, defaults apisconfig.Defaults) *netv1alpha1.HTTPIngressPath {
	// Optimistically allocate |targets| elements.
	splits := make([]netv1alpha1.IngressBackendSplit, 0, len(targets))

	var (
		// TODO: What should be the minimum duration?
		duration    time.Duration = time.Duration(defaults.RevisionTimeoutSeconds) * time.Second
		sawDuration               = false
	)

	for _, t := range targets {
		if t.Percent == nil || *t.Percent == 0 {
			continue
		}

		if t.Timeout != nil && duration.Nanoseconds() < t.Timeout.Nanoseconds() {
			duration = *t.Timeout
			sawDuration = true
		}

		splits = append(splits, netv1alpha1.IngressBackendSplit{
			IngressBackend: netv1alpha1.IngressBackend{
				ServiceNamespace: ns,
				ServiceName:      t.ServiceName,
				// Port on the public service must match port on the activator.
				// Otherwise, the serverless services can't guarantee seamless positive handoff.
				ServicePort: intstr.FromInt(networking.ServicePort(t.Protocol)),
			},
			Percent: int(*t.Percent),
			AppendHeaders: map[string]string{
				activator.RevisionHeaderName:      t.TrafficTarget.RevisionName,
				activator.RevisionHeaderNamespace: ns,
			},
		})
	}

	var timeout *metav1.Duration
	if sawDuration {
		timeout = &metav1.Duration{Duration: duration}
	}
	return &netv1alpha1.HTTPIngressPath{
		Splits:  splits,
		Timeout: timeout,
	}
}
