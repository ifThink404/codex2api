package api

import "github.com/gin-gonic/gin"

const errorObserverContextKey = "api_error_observer"

// ErrorObserver receives every standardized API error sent for a request.
// It must not write to the response; it is used for local diagnostics only.
type ErrorObserver func(*gin.Context, int, *APIError)

func SetErrorObserver(ctx *gin.Context, observer ErrorObserver) {
	ctx.Set(errorObserverContextKey, observer)
}

func ObserveError(ctx *gin.Context, status int, apiError *APIError) {
	if ctx == nil || apiError == nil {
		return
	}
	value, _ := ctx.Get(errorObserverContextKey)
	if observer, ok := value.(ErrorObserver); ok && observer != nil {
		observer(ctx, status, apiError)
	}
}
