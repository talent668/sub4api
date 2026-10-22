package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type scheduledHandlerPlanRepo struct {
	service.ScheduledTestPlanRepository
	plan                      *service.ScheduledTestPlan
	creates, updates, deletes int
}

func (r *scheduledHandlerPlanRepo) GetByID(context.Context, int64) (*service.ScheduledTestPlan, error) {
	return r.plan, nil
}
func (r *scheduledHandlerPlanRepo) Create(_ context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	r.creates++
	r.plan = plan
	return plan, nil
}
func (r *scheduledHandlerPlanRepo) Update(_ context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	r.updates++
	r.plan = plan
	return plan, nil
}
func (r *scheduledHandlerPlanRepo) Delete(context.Context, int64) error {
	r.deletes++
	return nil
}

type scheduledHandlerKeyRepo struct {
	service.APIKeyRepository
	key   *service.APIKey
	calls int
}

func (r *scheduledHandlerKeyRepo) GetByID(context.Context, int64) (*service.APIKey, error) {
	r.calls++
	if r.key == nil {
		return nil, errors.New("key not found")
	}
	return r.key, nil
}

func newScheduledHandlerTest(t *testing.T, method, body string, plans *scheduledHandlerPlanRepo, keys *scheduledHandlerKeyRepo, userID int64) (*ScheduledTestHandler, *gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	h := NewScheduledTestHandler(service.NewScheduledTestService(plans, nil), service.NewAPIKeyService(keys, nil, nil, nil, nil, nil, nil))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/api/v1/admin/scheduled-test-plans/1", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	if userID > 0 {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID})
	}
	return h, c, w
}

func TestScheduledDiagnosticCreateAuthorizesBillingKey(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	for _, tt := range []struct {
		name   string
		key    *service.APIKey
		userID int64
		status int
	}{
		{"own key", &service.APIKey{UserID: 17, Key: "test-key", Status: service.StatusActive}, 17, http.StatusOK},
		{"another admin key", &service.APIKey{UserID: 18, Key: "test-key", Status: service.StatusActive}, 17, http.StatusForbidden},
		{"missing auth", &service.APIKey{UserID: 17, Key: "test-key", Status: service.StatusActive}, 0, http.StatusForbidden},
		{"deleted key", nil, 17, http.StatusForbidden},
		{"disabled key", &service.APIKey{UserID: 17, Key: "test-key", Status: service.StatusAPIKeyDisabled}, 17, http.StatusForbidden},
		{"expired key", &service.APIKey{UserID: 17, Key: "test-key", Status: service.StatusActive, ExpiresAt: &expired}, 17, http.StatusForbidden},
		{"exhausted key", &service.APIKey{UserID: 17, Key: "test-key", Status: service.StatusActive, Quota: 1, QuotaUsed: 1}, 17, http.StatusForbidden},
		{"empty credential", &service.APIKey{UserID: 17, Status: service.StatusActive}, 17, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plans := &scheduledHandlerPlanRepo{}
			h, c, w := newScheduledHandlerTest(t, http.MethodPost, `{"account_id":41,"test_type":" CODEX_DIAGNOSTIC ","api_key_id":9,"models":["gpt-5.4"],"cron_expression":"*/30 * * * *"}`, plans, &scheduledHandlerKeyRepo{key: tt.key}, tt.userID)
			h.Create(c)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			if tt.status == http.StatusOK {
				require.Equal(t, 1, plans.creates)
				require.Equal(t, service.ScheduledTestTypeCodexDiagnostic, plans.plan.TestType)
			} else {
				require.Zero(t, plans.creates)
				require.Nil(t, plans.plan)
			}
		})
	}
}

func TestScheduledDiagnosticUpdateAuthorizesBillingChanges(t *testing.T) {
	for _, tt := range []struct {
		name         string
		previousType string
		wasEnabled   bool
		body         string
		keyOwner     int64
		status       int
	}{
		{"replace with another admin key", service.ScheduledTestTypeCodexDiagnostic, true, `{"api_key_id":10}`, 18, http.StatusForbidden},
		{"replace with own key", service.ScheduledTestTypeCodexDiagnostic, true, `{"api_key_id":10}`, 17, http.StatusOK},
		{"switch type with another admin key", service.ScheduledTestTypeAccountTest, true, `{"test_type":" CODEX_DIAGNOSTIC ","api_key_id":10,"models":["gpt-5.4"]}`, 18, http.StatusForbidden},
		{"switch type with own key", service.ScheduledTestTypeAccountTest, true, `{"test_type":"codex_diagnostic","api_key_id":10,"models":["gpt-5.4"]}`, 17, http.StatusOK},
		{"enable another admin key", service.ScheduledTestTypeCodexDiagnostic, false, `{"enabled":true}`, 18, http.StatusForbidden},
		{"enable own key", service.ScheduledTestTypeCodexDiagnostic, false, `{"enabled":true}`, 17, http.StatusOK},
		{"edit enabled foreign plan cron without replacing key", service.ScheduledTestTypeCodexDiagnostic, true, `{"cron_expression":"* * * * *"}`, 18, http.StatusForbidden},
		{"edit enabled foreign plan models without replacing key", service.ScheduledTestTypeCodexDiagnostic, true, `{"models":["gpt-5.4","gpt-5.5"]}`, 18, http.StatusForbidden},
		{"edit enabled own plan cron without replacing key", service.ScheduledTestTypeCodexDiagnostic, true, `{"cron_expression":"* * * * *"}`, 17, http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			keyID := int64(9)
			original := &service.ScheduledTestPlan{ID: 1, AccountID: 41, TestType: tt.previousType, APIKeyID: &keyID, ModelID: "gpt-5.4", Models: []string{"gpt-5.4"}, CronExpression: "*/30 * * * *", Enabled: tt.wasEnabled}
			plans := &scheduledHandlerPlanRepo{plan: original}
			keys := &scheduledHandlerKeyRepo{key: &service.APIKey{UserID: tt.keyOwner, Key: "test-key", Status: service.StatusActive}}
			h, c, w := newScheduledHandlerTest(t, http.MethodPut, tt.body, plans, keys, 17)
			h.Update(c)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			require.Equal(t, 1, keys.calls)
			if tt.status == http.StatusOK {
				require.Equal(t, 1, plans.updates)
			} else {
				require.Zero(t, plans.updates)
				require.Same(t, original, plans.plan)
				require.Equal(t, tt.previousType, original.TestType)
				require.EqualValues(t, 9, *original.APIKeyID)
				require.Equal(t, tt.wasEnabled, original.Enabled)
			}
		})
	}
}

func TestScheduledDiagnosticInvalidKeyPlanCanBeDisabledOrDeleted(t *testing.T) {
	for _, body := range []string{`{"enabled":false}`, `{"test_type":"codex_diagnostic","api_key_id":9,"models":["gpt-5.4"],"enabled":false}`} {
		t.Run(body, func(t *testing.T) {
			keyID := int64(9)
			plans := &scheduledHandlerPlanRepo{plan: &service.ScheduledTestPlan{ID: 1, TestType: service.ScheduledTestTypeCodexDiagnostic, APIKeyID: &keyID, Models: []string{"gpt-5.4"}, CronExpression: "*/30 * * * *", Enabled: true}}
			keys := &scheduledHandlerKeyRepo{}
			h, c, w := newScheduledHandlerTest(t, http.MethodPut, body, plans, keys, 17)
			h.Update(c)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, 1, plans.updates)
			require.False(t, plans.plan.Enabled)
			require.Zero(t, keys.calls)
		})
	}
	plans, keys := &scheduledHandlerPlanRepo{}, &scheduledHandlerKeyRepo{}
	h, c, w := newScheduledHandlerTest(t, http.MethodDelete, "", plans, keys, 17)
	h.Delete(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, plans.deletes)
	require.Zero(t, keys.calls)
}

func TestScheduledDiagnosticDeletedKeyPlanCanBeDisabled(t *testing.T) {
	plans := &scheduledHandlerPlanRepo{plan: &service.ScheduledTestPlan{
		ID: 1, TestType: service.ScheduledTestTypeCodexDiagnostic,
		APIKeyID: nil, Models: []string{"gpt-5.4"}, CronExpression: "*/30 * * * *", Enabled: true,
	}}
	keys := &scheduledHandlerKeyRepo{}
	h, c, w := newScheduledHandlerTest(t, http.MethodPut, `{"enabled":false}`, plans, keys, 17)
	h.Update(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, plans.updates)
	require.False(t, plans.plan.Enabled)
	require.Nil(t, plans.plan.APIKeyID)
	require.Zero(t, keys.calls)
}

func TestScheduledAccountTestDoesNotRequireBillingKey(t *testing.T) {
	plans, keys := &scheduledHandlerPlanRepo{}, &scheduledHandlerKeyRepo{}
	h, c, w := newScheduledHandlerTest(t, http.MethodPost, `{"account_id":41,"model_id":"gpt-5.4","cron_expression":"*/30 * * * *"}`, plans, keys, 17)
	h.Create(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, plans.creates)
	require.Zero(t, keys.calls)
}
