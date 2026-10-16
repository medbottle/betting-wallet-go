package httpapi

import "net/http"

// errorBody is the error response of ARCHITECTURE §13:
// {"error":{"code","message","retryable"}}.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func writeError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message, Retryable: retryable}})
}
