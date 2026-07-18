package webhook

import "net/http"

// AuthAttempt records a single authentication method that was attempted.
type AuthAttempt struct {
	Type     string `json:"type"`
	Verified bool   `json:"verified"`
}

// AuthResult records the overall outcome of webhook authentication.
type AuthResult struct {
	Required bool          `json:"required"`
	Methods  []AuthAttempt `json:"methods"`
	Success  bool          `json:"success"`
}

// authResultSetter is implemented by response writers that can store auth results.
type authResultSetter interface {
	SetAuthResult(result *AuthResult)
}

// SetAuthOnResponse stores the auth result on the response writer if it supports it.
func SetAuthOnResponse(w http.ResponseWriter, result *AuthResult) {
	if setter, ok := w.(authResultSetter); ok {
		setter.SetAuthResult(result)
	}
}
