package main

// Minimal authentication.k8s.io/v1 TokenReview wire types. Hand-rolled to avoid
// pulling k8s.io/api into the spinifex module for these few fields; the shapes
// match what kube-apiserver sends and expects on the webhook contract.

type tokenReview struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Spec       tokenReviewSpec   `json:"spec,omitzero"`
	Status     tokenReviewStatus `json:"status"`
}

type tokenReviewSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences,omitempty"`
}

type tokenReviewStatus struct {
	Authenticated bool     `json:"authenticated"`
	User          userInfo `json:"user,omitzero"`
	// Audiences echoes the request's spec.audiences on success. When the
	// apiserver runs with --api-audiences, it sends spec.audiences and discards
	// an authenticated=true response that does not confirm a matching audience —
	// so an empty Audiences here surfaces as a 401 despite a valid principal.
	Audiences []string `json:"audiences,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type userInfo struct {
	Username string   `json:"username,omitempty"`
	UID      string   `json:"uid,omitempty"`
	Groups   []string `json:"groups,omitempty"`
}
