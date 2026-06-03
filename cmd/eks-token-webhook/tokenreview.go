package main

// Minimal authentication.k8s.io/v1 TokenReview wire types. Hand-rolled to avoid
// pulling k8s.io/api into the spinifex module for these few fields; the shapes
// match what kube-apiserver sends and expects on the webhook contract.

type tokenReview struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Spec       tokenReviewSpec   `json:"spec"`
	Status     tokenReviewStatus `json:"status"`
}

type tokenReviewSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences,omitempty"`
}

type tokenReviewStatus struct {
	Authenticated bool     `json:"authenticated"`
	User          userInfo `json:"user,omitzero"`
	Error         string   `json:"error,omitempty"`
}

type userInfo struct {
	Username string   `json:"username,omitempty"`
	UID      string   `json:"uid,omitempty"`
	Groups   []string `json:"groups,omitempty"`
}
