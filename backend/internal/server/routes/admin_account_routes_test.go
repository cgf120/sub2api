package routes

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRegisterAccountRoutesStaticPathsDoNotConflictWithIDRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminGroup := router.Group("/api/v1/admin")
	handlers := &handler.Handlers{
		Admin: &handler.AdminHandlers{
			Account: adminhandler.NewAccountHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil),
			OAuth:   adminhandler.NewOAuthHandler(nil),
		},
	}

	require.NotPanics(t, func() {
		registerAccountRoutes(adminGroup, handlers)
	})

	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/admin/accounts/bulk-import/:job_id"},
		{http.MethodPost, "/api/v1/admin/accounts/batch-test"},
		{http.MethodGet, "/api/v1/admin/accounts/batch-test/:job_id"},
		{http.MethodPost, "/api/v1/admin/accounts/today-stats/batch"},
		{http.MethodPost, "/api/v1/admin/accounts/models/sync-upstream-preview"},
		{http.MethodGet, "/api/v1/admin/accounts/data"},
		{http.MethodGet, "/api/v1/admin/accounts/:id"},
		{http.MethodPost, "/api/v1/admin/accounts/:id/test"},
	} {
		require.True(t, hasRoute(router, route.method, route.path), "missing route %s %s", route.method, route.path)
	}
}

func hasRoute(router *gin.Engine, method, path string) bool {
	for _, route := range router.Routes() {
		if route.Method == method && route.Path == path {
			return true
		}
	}
	return false
}
