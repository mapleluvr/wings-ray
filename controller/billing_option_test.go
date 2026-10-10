package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type quotaCompletionFault struct {
	client          *redis.Client
	cacheKey, phase string
	armed           atomic.Bool
	recovered       chan struct{}
	credits         chan struct{}
	once            sync.Once
}

func (h *quotaCompletionFault) matches(cmd redis.Cmder) bool {
	args := cmd.Args()
	return cmd.Name() == "eval" && len(args) > 3 && strings.Contains(fmt.Sprint(args[1]), "'pending', -1") && fmt.Sprint(args[3]) == h.cacheKey
}
func (h *quotaCompletionFault) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if h.phase == "outage" && cmd.Name() == "eval" && strings.Contains(fmt.Sprint(cmd.Args()[1]), "'Quota', ARGV[1]") {
		h.credits <- struct{}{}
	}
	if h.phase == "after" && h.armed.Load() {
		args := cmd.Args()
		if cmd.Name() == "eval" && len(args) > 3 && fmt.Sprint(args[3]) == h.cacheKey && (strings.Contains(fmt.Sprint(args[1]), "local quota =") || strings.Contains(fmt.Sprint(args[1]), "local remain =")) {
			return ctx, errors.New("controlled unavailable balance cache")
		}
	}
	if h.phase == "missing" && h.armed.Load() && h.matches(cmd) {
		if err := h.client.Del(ctx, h.cacheKey).Err(); err != nil {
			return ctx, err
		}
	}
	if h.phase == "before" && h.armed.Load() && h.matches(cmd) {
		return ctx, errors.New("controlled quota completion failure")
	}
	return ctx, nil
}
func (h *quotaCompletionFault) AfterProcess(_ context.Context, cmd redis.Cmder) error {
	if !h.matches(cmd) {
		return nil
	}
	if h.phase == "after" && h.armed.Load() {
		return errors.New("controlled lost quota completion reply")
	}
	if !h.armed.Load() {
		h.once.Do(func() { close(h.recovered) })
	}
	return nil
}
func (h *quotaCompletionFault) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h *quotaCompletionFault) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func TestUpdateOptionRejectsInvalidTaskBillingExpressions(t *testing.T) {
	const pluginKey = "billing-save-probe"
	const modelName = "billing-save-model"
	source := `
export const meta = {
  apiVersion: 1, key: "billing-save-probe", name: "Billing Save Probe", version: "1.0.0", author: {name: "Test"},
  models: ["billing-save-model"], fetchMode: "per_task",
  usageSchema: {seconds: {type: "number", unit: "second"}}
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`
	_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(pluginKey) })

	tests := []struct {
		name       string
		expression string
		errorText  string
	}{
		{
			name:       "invalid syntax",
			expression: `tier("base",`,
			errorText:  "expr compile error",
		},
		{
			name:       "undeclared usage key",
			expression: `tier("base", u("clips") * 0.1)`,
			errorText:  `usage key \"clips\" is not declared`,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			expressions, marshalErr := common.Marshal(map[string]string{modelName: testCase.expression})
			require.NoError(t, marshalErr)
			body, marshalErr := common.Marshal(OptionUpdateRequest{
				Key:   "billing_setting.billing_expr",
				Value: string(expressions),
			})
			require.NoError(t, marshalErr)
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPut, "/api/option/", strings.NewReader(string(body)))

			UpdateOption(context)

			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.Contains(t, recorder.Body.String(), `"success":false`)
			assert.Contains(t, recorder.Body.String(), modelName)
			assert.Contains(t, recorder.Body.String(), testCase.errorText)
		})
	}
}

func TestUpdateOptionRejectsUsageExpressionWithoutTaskPlugin(t *testing.T) {
	const modelName = "billing-save-model-without-plugin"
	expressions, err := common.Marshal(map[string]string{
		modelName: `u("mode") == "std" ? 1 : 2`,
	})
	require.NoError(t, err)
	body, err := common.Marshal(OptionUpdateRequest{
		Key:   "billing_setting.billing_expr",
		Value: string(expressions),
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/",
		strings.NewReader(string(body)),
	)

	UpdateOption(context)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"success":false`)
	assert.Contains(t, recorder.Body.String(), modelName)
	assert.Contains(t, recorder.Body.String(), "mode")
	assert.Contains(t, recorder.Body.String(), "no task plugin usage schema")
}

func TestUpdateOptionAliasBillingExprUsesPluginSchema(t *testing.T) {
	database := modelManagementDB(t, "sqlite", "")
	require.NoError(t, database.AutoMigrate(&model.Log{}))
	const pluginKey = "billing-alias-probe"
	source := `
export const meta = {
  apiVersion: 1, key: "billing-alias-probe", name: "Billing Alias Probe", version: "1.0.0", author: {name: "Test"},
  models: ["declared-model"], fetchMode: "per_task",
  usageSchema: {seconds: {type: "number", unit: "second"}, image_count: {type: "number", unit: "count"}},
  usageProfiles: [{models: ["declared-model"], schema: {seconds: {type: "number", unit: "second"}}}]
};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {}; }
`
	_, err := jsplugin.DefaultRegistry.Register(source, jsplugin.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { jsplugin.DefaultRegistry.Unregister(pluginKey) })

	mapping := `{"alias-model":"declared-model"}`
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:           1,
		Type:         54,
		Key:          "key-1",
		Status:       common.ChannelStatusEnabled,
		Name:         "ch-1",
		Group:        "default",
		Models:       "alias-model,declared-model",
		ModelMapping: &mapping,
	}).Error)
	model.InitChannelCache()

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})

	putExpr := func(modelName, expression string) *httptest.ResponseRecorder {
		t.Helper()
		expressions, marshalErr := common.Marshal(map[string]string{modelName: expression})
		require.NoError(t, marshalErr)
		body, marshalErr := common.Marshal(OptionUpdateRequest{
			Key:   "billing_setting.billing_expr",
			Value: string(expressions),
		})
		require.NoError(t, marshalErr)
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodPut, "/api/option/", strings.NewReader(string(body)))
		UpdateOption(context)
		return recorder
	}

	accepted := putExpr("alias-model", `u("seconds")`)
	assert.Equal(t, http.StatusOK, accepted.Code)
	assert.Contains(t, accepted.Body.String(), `"success":true`)

	rejectedKey := putExpr("alias-model", `u("image_count")`)
	assert.Equal(t, http.StatusOK, rejectedKey.Code)
	assert.Contains(t, rejectedKey.Body.String(), `"success":false`)
	assert.Contains(t, rejectedKey.Body.String(), `usage key \"image_count\" is not declared`)

	rejectedDeclared := putExpr("declared-model", `u("image_count")`)
	assert.Contains(t, rejectedDeclared.Body.String(), `"success":false`)
	assert.Contains(t, rejectedDeclared.Body.String(), `usage key \"image_count\" is not declared`)

	unresolvable := putExpr("unknown-alias-model", `u("seconds")`)
	assert.Equal(t, http.StatusOK, unresolvable.Code)
	assert.Contains(t, unresolvable.Body.String(), `"success":false`)
	assert.Contains(t, unresolvable.Body.String(), "no task plugin usage schema")
}

// Covers configuration persistence, validation, reservation and reconciliation
// through the same public entry points used by the relay.
func TestPreConsumePolicyDatabaseMatrix(t *testing.T) {
	previousConfig := config.GlobalConfig.ExportAllConfigs()
	previousUnit, previousBatch := common.QuotaPerUnit, common.BatchUpdateEnabled
	common.QuotaPerUnit, common.BatchUpdateEnabled = 500000, false
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(previousConfig))
		common.QuotaPerUnit, common.BatchUpdateEnabled = previousUnit, previousBatch
	})
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip("set " + dialect.env + " to run this database")
			}
			db := modelManagementDB(t, dialect.kind, os.Getenv(dialect.env))
			require.NoError(t, db.AutoMigrate(&model.Token{}))
			for key, value := range map[string]string{
				"quota_setting.trust_quota_usd":        "10.5",
				"quota_setting.pre_consume_multiplier": "1.5",
			} {
				var response struct{ Success bool }
				modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: key, Value: value}, &response)
				require.True(t, response.Success)
				var saved model.Option
				require.NoError(t, db.Where(&model.Option{Key: key}).First(&saved).Error)
				assert.Equal(t, value, saved.Value)
			}
			var savedOptions []model.Option
			require.NoError(t, db.Find(&savedOptions).Error)
			stored := map[string]string{}
			for _, option := range savedOptions {
				stored[option.Key] = option.Value
			}
			for range 2 {
				operation_setting.GetQuotaSetting().TrustQuotaUSD = 10
				operation_setting.GetQuotaSetting().PreConsumeMultiplier = 1
				require.NoError(t, config.GlobalConfig.LoadFromDB(stored))
				assert.Equal(t, 10.5, operation_setting.GetQuotaSetting().TrustQuotaUSD)
				assert.Equal(t, 1.5, operation_setting.GetQuotaSetting().PreConsumeMultiplier)
			}
			for _, tc := range []struct{ key, value string }{
				{"trust_quota_usd", "-1"}, {"trust_quota_usd", "NaN"}, {"trust_quota_usd", "+Inf"},
				{"pre_consume_multiplier", "0"}, {"pre_consume_multiplier", "-0.5"},
				{"pre_consume_multiplier", "NaN"}, {"pre_consume_multiplier", "Inf"},
				{"pre_consume_multiplier", "1e309"}, {"pre_consume_multiplier", ""},
			} {
				key := "quota_setting." + tc.key
				var response struct{ Success bool }
				modelManagementRequest(t, UpdateOption, http.MethodPut, "/api/option/", OptionUpdateRequest{Key: key, Value: tc.value}, &response)
				assert.False(t, response.Success, "%s=%q", key, tc.value)
				var saved model.Option
				require.NoError(t, db.Where(&model.Option{Key: key}).First(&saved).Error)
				assert.Equal(t, stored[key], saved.Value, "invalid saves must preserve the previous value")
			}
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
				"billing_setting.billing_mode":    `{"policy-model":"tiered_expr"}`,
				"billing_setting.billing_expr":    `{"policy-model":"tier(\"base\", p * 3 + c * 15)"}`,
				"group_ratio_setting.group_ratio": `{"default":1}`,
			}))
			cases := []struct {
				name                  string
				threshold, multiplier float64
				wallet, token         int
				force, unlimited      bool
				wantHeld              int
			}{
				{"above threshold", 10, 1, 5500000, 5500000, false, false, 0},
				{"custom threshold", 20, 1, 5500000, 5500000, false, false, 1500},
				{"zero disables bypass", 0, 1, 5500000, 5500000, false, false, 1500},
				{"wallet equals threshold", 10, 1, 5000000, 5500000, false, false, 1500},
				{"token equals threshold", 10, 1, 5500000, 5000000, false, false, 1500},
				{"fractional threshold", 10.5, 1, 5250001, 5250001, false, false, 0},
				{"unlimited token", 10, 1, 5500000, 0, false, true, 0},
				{"forced reservation", 10, 1, 5500000, 5500000, true, false, 1500},
				{"half input cost", 0, 0.5, 100000, 100000, false, false, 750},
				{"fractional multiple refunds excess", 0, 2.5, 100000, 100000, false, false, 3750},
			}
			for i, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					require.NoError(t, model.UpdateOption("quota_setting.trust_quota_usd", strconv.FormatFloat(tc.threshold, 'f', -1, 64)))
					require.NoError(t, model.UpdateOption("quota_setting.pre_consume_multiplier", strconv.FormatFloat(tc.multiplier, 'f', -1, 64)))
					user := model.User{Username: fmt.Sprintf("policy-user-%d", i), Quota: tc.wallet, Group: "default", AffCode: fmt.Sprintf("policy-aff-%d", i)}
					require.NoError(t, db.Create(&user).Error)
					token := model.Token{UserId: user.Id, Key: fmt.Sprintf("policy-token-%d", i), RemainQuota: tc.token, UnlimitedQuota: tc.unlimited}
					require.NoError(t, db.Create(&token).Error)
					ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
					ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
					ctx.Set("token_quota", tc.token)
					info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, TokenUnlimited: tc.unlimited, ForcePreConsume: tc.force, OriginModelName: "policy-model", UserGroup: "default", UsingGroup: "default", BillingRequestInput: &billingexpr.RequestInput{}}
					info.UserSetting.BillingPreference = "wallet_only"
					price, err := helper.ModelPriceHelper(ctx, info, 1000, &types.TokenCountMeta{MaxTokens: 10000})
					require.NoError(t, err)
					require.Nil(t, service.PreConsumeBilling(ctx, price.QuotaToPreConsume, info))
					assert.Equal(t, tc.wantHeld, info.FinalPreConsumedQuota)
					require.NoError(t, db.First(&user, user.Id).Error)
					assert.Equal(t, tc.wallet-tc.wantHeld, user.Quota)
					if !tc.unlimited {
						require.NoError(t, db.First(&token, token.Id).Error)
						assert.Equal(t, tc.token-tc.wantHeld, token.RemainQuota)
					}
					_, actual, _ := service.TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 100, Len: 1000})
					assert.Equal(t, 2250, actual)
					require.NoError(t, info.Billing.Settle(actual))
					require.NoError(t, info.Billing.Settle(actual))
					require.NoError(t, db.First(&user, user.Id).Error)
					assert.Equal(t, tc.wallet-actual, user.Quota)
					if !tc.unlimited {
						require.NoError(t, db.First(&token, token.Id).Error)
						assert.Equal(t, tc.token-actual, token.RemainQuota)
					}
				})
			}
		})
	}
}

func TestPreConsumeMultiplierRejectsInvalidRuntimeAndOverflow(t *testing.T) {
	previous := config.GlobalConfig.ExportAllConfigs()
	t.Cleanup(func() { require.NoError(t, config.GlobalConfig.LoadFromDB(previous)) })
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":    `{"policy-overflow":"tiered_expr"}`,
		"billing_setting.billing_expr":    `{"policy-overflow":"tier(\"base\", p * 3)"}`,
		"group_ratio_setting.group_ratio": `{"default":1}`,
	}))
	for _, multiplier := range []float64{0, -0.5, math.NaN(), math.Inf(1), math.MaxFloat64} {
		operation_setting.GetQuotaSetting().PreConsumeMultiplier = multiplier
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		info := &relaycommon.RelayInfo{OriginModelName: "policy-overflow", UserGroup: "default", UsingGroup: "default", BillingRequestInput: &billingexpr.RequestInput{}}
		_, err := helper.ModelPriceHelper(ctx, info, 1000, &types.TokenCountMeta{})
		require.Error(t, err)
		assert.Nil(t, info.Billing)
	}
}

// Protects the real pricing -> reservation -> text settlement -> consume log
// contract: funding policy changes the user debit, never the channel list cost.
func TestFundingMultiplierDatabaseMatrix(t *testing.T) {
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousStreamingTimeout })
	previousConfig := config.GlobalConfig.ExportAllConfigs()
	previousUnit, previousBatch, previousLog := common.QuotaPerUnit, common.BatchUpdateEnabled, common.LogConsumeEnabled
	common.QuotaPerUnit, common.BatchUpdateEnabled, common.LogConsumeEnabled = 500000, false, true
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(previousConfig))
		common.QuotaPerUnit, common.BatchUpdateEnabled, common.LogConsumeEnabled = previousUnit, previousBatch, previousLog
	})

	if os.Getenv("TEST_BATCH_ACCOUNTING") == "true" {
		previousInterval := common.BatchUpdateInterval
		common.BatchUpdateInterval = 1
		model.InitBatchUpdater()
		t.Cleanup(func() { common.BatchUpdateInterval = previousInterval })
	}
	if addr := os.Getenv("TEST_REDIS_ADDR"); addr != "" {
		previousClient := common.RDB
		client := redis.NewClient(&redis.Options{Addr: addr, DB: 15})
		require.NoError(t, client.Ping(t.Context()).Err())
		common.RDB = client
		t.Cleanup(func() { require.NoError(t, client.Close()); common.RDB = previousClient })
	}
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip("set " + dialect.env + " to run this database")
			}
			db := modelManagementDB(t, dialect.kind, os.Getenv(dialect.env))
			if os.Getenv("TEST_REDIS_ADDR") != "" {
				require.NoError(t, common.RDB.FlushDB(t.Context()).Err())
				common.RedisEnabled = true
			}
			common.BatchUpdateEnabled = os.Getenv("TEST_BATCH_ACCOUNTING") == "true"
			if logDSN := os.Getenv("TEST_LOG_DSN"); logDSN != "" {
				t.Setenv("LOG_SQL_DSN", logDSN)
				previousMaster := common.IsMasterNode
				common.IsMasterNode = true
				require.NoError(t, model.InitLogDB())
				require.NoError(t, model.InitLogDB())
				common.IsMasterNode = previousMaster
			}
			require.NoError(t, db.AutoMigrate(&model.Token{}, &model.Log{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}, &model.Checkin{}))
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
				"group_ratio_setting.group_ratio":      `{"default":1}`,
				"quota_setting.trust_quota_usd":        "0",
				"quota_setting.pre_consume_multiplier": "1",
			}))

			t.Run("save and stale reload publish complete snapshots", func(t *testing.T) {
				const name = "publication-model"
				require.NoError(t, model.UpdateModelPricingOptions(map[string]string{
					"billing_setting.billing_mode":            `{"publication-model":"tiered_expr"}`,
					"billing_setting.billing_expr":            `{"publication-model":"p * 2"}`,
					"billing_setting.subscription_multiplier": `{"publication-model":0.5}`,
					"billing_setting.wallet_multiplier":       `{"publication-model":0.9}`,
				}))
				read, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				require.NoError(t, db.Callback().Query().After("gorm:query").Register("pause_funding_reload", func(tx *gorm.DB) {
					if _, ok := tx.Statement.Dest.(*[]*model.Option); !ok {
						return
					}
					once.Do(func() { close(read); <-release })
				}))
				defer func() { require.NoError(t, db.Callback().Query().Remove("pause_funding_reload")) }()
				reloadDone := make(chan struct{})
				go func() { model.InitOptionMap(); close(reloadDone) }()
				select {
				case <-read:
				case <-time.After(5 * time.Second):
					t.Fatal("option reload did not reach its database read")
				}
				saved := make(chan error, 1)
				go func() {
					saved <- model.UpdateModelPricingOptions(map[string]string{
						"billing_setting.billing_expr":            `{"publication-model":"p * 8"}`,
						"billing_setting.subscription_multiplier": `{"publication-model":1.5}`,
						"billing_setting.wallet_multiplier":       `{"publication-model":2}`,
					})
				}()
				// Hold the stale read until either an unprotected save completes
				// or the serialized save is blocked. This timeout only releases
				// the deliberate query barrier; no duration is asserted.
				var saveErr error
				completed := false
				select {
				case saveErr = <-saved:
					completed = true
				case <-time.After(200 * time.Millisecond):
				}
				close(release)
				<-reloadDone
				if !completed {
					saveErr = <-saved
				}
				require.NoError(t, saveErr)
				actual := billing_setting.GetModelBillingConfig(name)
				assert.Equal(t, "p * 8", actual.Expression)
				assert.Equal(t, 1.5, actual.SubscriptionMultiplier)
				assert.Equal(t, float64(2), actual.WalletMultiplier)
			})
			t.Run("legacy rewards survive a Redis outage", func(t *testing.T) {
				previousClient, previousRedis := common.RDB, common.RedisEnabled
				disconnected := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
				fault := &quotaCompletionFault{phase: "outage", credits: make(chan struct{}, 2)}
				disconnected.AddHook(fault)
				require.NoError(t, disconnected.Close())
				common.RDB, common.RedisEnabled = disconnected, true
				t.Cleanup(func() { common.RDB, common.RedisEnabled = previousClient, previousRedis })
				previousNew, previousInvitee, previousInviter := common.QuotaForNewUser, common.QuotaForInvitee, common.QuotaForInviter
				common.QuotaForNewUser, common.QuotaForInvitee, common.QuotaForInviter = 0, 25, 0
				payment, checkin := operation_setting.GetPaymentSetting(), operation_setting.GetCheckinSetting()
				previousPayment, previousCheckin := *payment, *checkin
				payment.ComplianceConfirmed, payment.ComplianceTermsVersion = true, operation_setting.CurrentComplianceTermsVersion
				checkin.Enabled, checkin.MinQuota, checkin.MaxQuota = true, 25, 25
				t.Cleanup(func() {
					common.QuotaForNewUser, common.QuotaForInvitee, common.QuotaForInviter = previousNew, previousInvitee, previousInviter
					*payment, *checkin = previousPayment, previousCheckin
				})
				invitee := model.User{Username: "outage-invitee", Quota: 100, Group: "default", AffCode: "outage-invitee", AuthVersion: 1}
				require.NoError(t, db.Create(&invitee).Error)
				var beforeRewards, afterRewards int64
				rewardQuery := model.LOG_DB.Model(&model.Log{}).Where("user_id = ? AND content LIKE ?", invitee.Id, "使用邀请码赠送%")
				require.NoError(t, rewardQuery.Count(&beforeRewards).Error)
				invitee.FinishInsert(invitee.Id)
				require.NoError(t, db.First(&invitee, invitee.Id).Error)
				assert.Equal(t, 125, invitee.Quota, "an award log must have its SQL credit")
				require.NoError(t, rewardQuery.Count(&afterRewards).Error)
				assert.Equal(t, beforeRewards+1, afterRewards)
				user := model.User{Username: "outage-checkin", Quota: 100, Group: "default", AffCode: "outage-checkin", AuthVersion: 1}
				require.NoError(t, db.Create(&user).Error)
				record, err := model.UserCheckin(user.Id)
				require.NoError(t, err)
				assert.Equal(t, 25, record.QuotaAwarded)
				require.NoError(t, db.First(&user, user.Id).Error)
				assert.Equal(t, 125, user.Quota)
				for range 2 {
					select {
					case <-fault.credits:
					case <-time.After(5 * time.Second):
						t.Fatal("reward cache attempt did not finish")
					}
				}
			})
			if common.RedisEnabled && common.BatchUpdateEnabled {
				for _, failure := range []struct {
					phase          string
					token, failSQL bool
				}{{"before", false, false}, {"after", false, false}, {"missing", false, false}, {"before", true, false}, {"after", true, false}, {"missing", true, false}, {"missing", false, true}, {"missing", true, true}} {
					t.Run(fmt.Sprintf("uncertain %s completion token=%t sql-error=%t", failure.phase, failure.token, failure.failSQL), func(t *testing.T) {
						suffix := fmt.Sprintf("%s-%t-%t", failure.phase, failure.token, failure.failSQL)
						want := 600
						if failure.failSQL {
							want = 700
						}
						user := model.User{Username: "reconcile-" + suffix, Quota: 1000, Group: "default", AffCode: "reconcile-" + suffix, AuthVersion: 1}
						require.NoError(t, db.Create(&user).Error)
						token := model.Token{UserId: user.Id, Key: "reconcile-" + suffix, Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}
						require.NoError(t, db.Create(&token).Error)
						_, err := model.GetUserCache(user.Id)
						require.NoError(t, err)
						_, err = model.GetTokenByKey(token.Key, false)
						require.NoError(t, err)
						release := make(chan struct{})
						var releaseOnce sync.Once
						t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
						callback := "hold-legacy-quota-flush"
						require.NoError(t, db.Callback().Update().Before("gorm:begin_transaction").Register(callback, func(tx *gorm.DB) {
							updates, ok := tx.Statement.Dest.(map[string]any)
							if !ok {
								return
							}
							field := "quota"
							if failure.token {
								field = "remain_quota"
							}
							expr, ok := updates[field].(clause.Expr)
							if failure.failSQL && ok && len(expr.Vars) > 0 && (expr.SQL == field+" - ?" && fmt.Sprint(expr.Vars[0]) == "100" || expr.SQL == field+" + ?" && fmt.Sprint(expr.Vars[0]) == "-100") {
								tx.AddError(errors.New("controlled aborted quota SQL adjustment"))
							}
							if ok && expr.SQL == field+" + ?" && len(expr.Vars) > 0 && fmt.Sprint(expr.Vars[0]) == "-300" {
								<-release
							}
						}))
						t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
						cacheKey := fmt.Sprintf("user:%d", user.Id)
						if failure.token {
							cacheKey = "token:" + common.GenerateHMAC(token.Key)
							reserved, err := model.TryReserveTokenQuota(token.Id, token.Key, 300, false)
							require.NoError(t, err)
							require.True(t, reserved)
						} else {
							reserved, err := model.TryReserveUserQuota(user.Id, 300)
							require.NoError(t, err)
							require.True(t, reserved)
						}
						snapshot, err := common.RDB.HGetAll(t.Context(), cacheKey).Result()
						require.NoError(t, err)
						client := redis.NewClient(common.RDB.Options())
						fault := &quotaCompletionFault{client: client, cacheKey: cacheKey, phase: failure.phase, recovered: make(chan struct{})}
						fault.armed.Store(true)
						client.AddHook(fault)
						previousClient := common.RDB
						common.RDB = client
						t.Cleanup(func() { common.RDB = previousClient; require.NoError(t, client.Close()) })
						ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
						ctx.Set("token_quota", 1000)
						info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, RequestId: common.NewRequestId(), UserGroup: "default", UsingGroup: "default", FundingPricing: &relaycommon.FundingPricing{Version: 1, Multiplier: 1, SubscriptionMultiplier: 1, WalletMultiplier: 1}, TieredBillingSnapshot: &billingexpr.BillingSnapshot{GroupRatio: 1}}
						info.UserSetting.BillingPreference = "wallet_only"
						require.Nil(t, service.PreConsumeBilling(ctx, 0, info))
						settleErr := info.Billing.Settle(100)
						if failure.failSQL {
							require.Error(t, settleErr)
						} else {
							require.NoError(t, settleErr)
						}
						if failure.token {
							reserved, _ := model.TryReserveTokenQuota(token.Id, token.Key, 800, false)
							assert.False(t, reserved, "SQL fallback must not spend an unflushed legacy reservation")
						} else {
							reserved, _ := model.TryReserveUserQuota(user.Id, 800)
							assert.False(t, reserved, "SQL fallback must not spend an unflushed legacy reservation")
						}
						fault.armed.Store(false)
						if failure.phase == "missing" {
							require.NoError(t, common.RDB.HSet(t.Context(), cacheKey, snapshot).Err())
						}
						select {
						case <-fault.recovered:
						case <-time.After(5 * time.Second):
							t.Fatal("idempotent cache-only recovery did not complete")
						}
						if failure.token {
							cached, err := model.GetTokenByKey(token.Key, false)
							require.NoError(t, err)
							assert.Equal(t, want, cached.RemainQuota)
						} else {
							cached, err := model.GetUserCache(user.Id)
							require.NoError(t, err)
							assert.Equal(t, want, cached.Quota)
						}
						releaseOnce.Do(func() { close(release) })
						require.Eventually(t, func() bool {
							if failure.token {
								var current model.Token
								return db.First(&current, token.Id).Error == nil && current.RemainQuota == want
							}
							var current model.User
							return db.First(&current, user.Id).Error == nil && current.Quota == want
						}, 5*time.Second, 10*time.Millisecond, "SQL is written once and the real legacy batch reservation flushes once")
					})
				}
			}
			if common.RedisEnabled {
				for _, cold := range []struct {
					name                 string
					token, refund, stale bool
				}{
					{name: "cold wallet debit"}, {name: "cold wallet refund", refund: true},
					{name: "cold token debit", token: true}, {name: "cold token refund", token: true, refund: true},
					{name: "stale wallet hydration", stale: true}, {name: "stale token hydration", token: true, stale: true},
				} {
					t.Run(cold.name, func(t *testing.T) {
						user := model.User{Username: strings.ReplaceAll(cold.name, " ", "-"), Quota: 1000, Group: "default", AffCode: strings.ReplaceAll(cold.name, " ", "-")}
						require.NoError(t, db.Create(&user).Error)
						token := model.Token{UserId: user.Id, Key: strings.ReplaceAll(cold.name, " ", ""), Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}
						require.NoError(t, db.Create(&token).Error)
						ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
						ctx.Set("token_quota", 1000)
						info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, UserGroup: "default", UsingGroup: "default", RequestId: common.NewRequestId(), FundingPricing: &relaycommon.FundingPricing{Version: 1, Multiplier: 1, SubscriptionMultiplier: 1, WalletMultiplier: 1}}
						info.UserSetting.BillingPreference = "wallet_only"
						held, final, want := 0, 100, 900
						if cold.refund {
							held, final, want = 100, 0, 1000
						}
						info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{EstimatedQuotaBeforeGroup: float64(held), EstimatedQuotaAfterGroup: held, GroupRatio: 1}
						require.Nil(t, service.PreConsumeBilling(ctx, held, info))
						cacheKey := fmt.Sprintf("user:%d", user.Id)
						table := "users"
						if cold.token {
							cacheKey = "token:" + common.GenerateHMAC(token.Key)
							table = "tokens"
						}
						require.NoError(t, common.RDB.Del(context.Background(), cacheKey).Err())
						callback := "cold-quota-hydration"
						if cold.stale {
							read, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
							require.NoError(t, db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
								if tx.Statement.Table != table {
									return
								}
								select {
								case <-read:
									return
								default:
									close(read)
								}
								<-release
							}))
							t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove(callback)) })
							go func() {
								if cold.token {
									_, err := model.GetTokenByKey(token.Key, false)
									done <- err
								} else {
									_, err := model.GetUserCache(user.Id)
									done <- err
								}
							}()
							<-read
							settleErr := info.Billing.Settle(final)
							close(release)
							require.NoError(t, settleErr)
							require.NoError(t, <-done)
							if cold.token {
								cached, err := model.GetTokenByKey(token.Key, false)
								require.NoError(t, err)
								assert.Equal(t, want, cached.RemainQuota)
							} else {
								cached, err := model.GetUserCache(user.Id)
								require.NoError(t, err)
								assert.Equal(t, want, cached.Quota)
							}
							return
						}
						observed, called := 0, false
						require.NoError(t, db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register(callback, func(tx *gorm.DB) {
							if called || tx.Statement.Table != table {
								return
							}
							called = true
							if cold.token {
								cached, err := model.GetTokenByKey(token.Key, false)
								if assert.NoError(t, err) {
									observed = cached.RemainQuota
								}
							} else {
								cached, err := model.GetUserCache(user.Id)
								if assert.NoError(t, err) {
									observed = cached.Quota
								}
							}
						}))
						t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
						require.NoError(t, info.Billing.Settle(final))
						require.True(t, called, "hydrate after the real database commit")
						assert.Equal(t, want, observed)
						require.Eventually(t, func() bool {
							if cold.token {
								cached, err := model.GetTokenByKey(token.Key, false)
								return err == nil && cached.RemainQuota == want
							}
							cached, err := model.GetUserCache(user.Id)
							return err == nil && cached.Quota == want
						}, time.Second, 5*time.Millisecond, "a committed quota must not be replayed into its newly hydrated snapshot")
					})
				}
			}
			for i, tc := range []struct {
				name, source, expression, preference, settlementFailure                           string
				subscription, wallet                                                              float64
				prompt                                                                            int
				usage                                                                             dto.Usage
				wantHeld, wantDebit, wantList, walletBalance, subBalance, wantDue                 int
				changeConfig, reject, retryGroup, trusted, httpRelay, stream, periodChanged       bool
				compact, audio, audioResponse, refund, resetBeforeRetry, deleteToken, toolFailure bool
				geminiAudio                                                                       string
			}{
				{name: "Gemini inline audio keeps original charge", source: "wallet", expression: `tier("base", p * 2 + c * 2)`, subscription: 1, wallet: 0.5, prompt: 1000, usage: dto.Usage{PromptTokens: 1000, TotalTokens: 1000}, wantHeld: 1000, wantDebit: 1000, wantList: 1000, geminiAudio: "inline"},
				{name: "Gemini file audio keeps original charge", source: "wallet", expression: `tier("base", p * 2 + c * 2)`, subscription: 1, wallet: 0.5, prompt: 1000, usage: dto.Usage{PromptTokens: 1000, TotalTokens: 1000}, wantHeld: 1000, wantDebit: 1000, wantList: 1000, geminiAudio: "file"},
				{name: "Gemini AUDIO output keeps original charge", source: "wallet", expression: `tier("base", p * 2 + c * 2)`, subscription: 1, wallet: 0.5, prompt: 1000, usage: dto.Usage{PromptTokens: 1000, TotalTokens: 1000}, wantHeld: 1000, wantDebit: 1000, wantList: 1000, geminiAudio: "output"},
				{name: "deleted token is not reported settled", source: "wallet", expression: `tier("base", p * 2 + c * 6)`, subscription: 1, wallet: 0.5, prompt: 100, usage: dto.Usage{PromptTokens: 100, CompletionTokens: 1000, TotalTokens: 1100}, wantHeld: 50, wantDebit: 1550, wantList: 3100, deleteToken: true},
				{name: "expression error keeps tool fee in list price", source: "wallet", expression: `p == 50 ? tier("error", param("missing") * p) : tier("base", p * 2)`, subscription: 1, wallet: 0.5, prompt: 100, usage: dto.Usage{PromptTokens: 50, TotalTokens: 50}, wantHeld: 50, wantDebit: 5050, wantList: 5100, toolFailure: true},
				{name: "Compact frozen wallet policy", source: "wallet", expression: `tier("base", p * 2)`, subscription: 1, wallet: 0.5, usage: dto.Usage{PromptTokens: 1000, TotalTokens: 1000}, wantDebit: 500, wantList: 1000, httpRelay: true, compact: true},
				{name: "Compact in-flight configuration stays frozen", source: "wallet", expression: `tier("base", p * 2)`, subscription: 1, wallet: 0.5, usage: dto.Usage{PromptTokens: 1000, TotalTokens: 1000}, wantDebit: 500, wantList: 1000, httpRelay: true, compact: true, changeConfig: true},
				{name: "audio Chat keeps original admission and charge", source: "wallet", expression: `tier("base", p * 2 + c * 2)`, subscription: 1, wallet: 1.5, prompt: 1000, usage: dto.Usage{PromptTokens: 1000, TotalTokens: 1000, PromptTokensDetails: dto.InputTokenDetails{AudioTokens: 1000}}, walletBalance: 1200, wantHeld: 1000, wantDebit: 1000, wantList: 1000, audio: true},
				{name: "audio Responses keeps original admission and charge", source: "wallet", expression: `tier("base", p * 2 + c * 2)`, subscription: 1, wallet: 1.5, prompt: 1000, usage: dto.Usage{PromptTokens: 1000, TotalTokens: 1000, PromptTokensDetails: dto.InputTokenDetails{AudioTokens: 1000}}, walletBalance: 1200, wantHeld: 1000, wantDebit: 1000, wantList: 1000, audio: true, audioResponse: true},
				{name: "retry refund cannot credit a new period", source: "subscription", expression: `tier("base", p * 2)`, subscription: 0.5, wallet: 1, prompt: 100, wantHeld: 100, retryGroup: true, periodChanged: true, refund: true},
				{name: "reset rejects supplemental reservation", source: "subscription", expression: `tier("base", p * 2)`, subscription: 0.5, wallet: 1, prompt: 100, periodChanged: true, resetBeforeRetry: true, refund: true},
				{name: "wallet commit failure retains only paid reservation", source: "wallet", expression: `tier("base", p * 2 + c * 6)`, subscription: 1, wallet: 0.5, prompt: 100, usage: dto.Usage{PromptTokens: 100, CompletionTokens: 1000, TotalTokens: 1100}, wantHeld: 50, wantDebit: 50, wantList: 3100, wantDue: 1550, settlementFailure: "wallet"},
				{name: "token failure keeps committed funding visible", source: "wallet", expression: `tier("base", p * 2 + c * 6)`, subscription: 1, wallet: 0.5, prompt: 100, usage: dto.Usage{PromptTokens: 100, CompletionTokens: 1000, TotalTokens: 1100}, wantHeld: 50, wantDebit: 1550, wantList: 3100, settlementFailure: "token"},
				{name: "subscription discount", source: "subscription", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, wallet: 0.9, prompt: 105000, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, wantHeld: 52500, wantDebit: 16500, wantList: 33000},
				{name: "wallet discount", source: "wallet", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, wallet: 0.9, prompt: 105000, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, wantHeld: 94500, wantDebit: 29700, wantList: 33000},
				{name: "surcharge", source: "wallet", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, wallet: 1.5, prompt: 105000, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, wantHeld: 157500, wantDebit: 49500, wantList: 33000},
				{name: "wallet does not inherit subscription", source: "wallet", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, prompt: 105000, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, wantHeld: 105000, wantDebit: 33000, wantList: 33000},
				{name: "independent rounding", source: "wallet", expression: `tier("base", p * 2.8)`, subscription: 1, wallet: 2.5, prompt: 1, usage: dto.Usage{PromptTokens: 1, TotalTokens: 1}, wantHeld: 4, wantDebit: 4, wantList: 1},
				{name: "request keeps frozen prices", source: "subscription", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, wallet: 0.9, prompt: 105000, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, wantHeld: 52500, wantDebit: 16500, wantList: 33000, changeConfig: true},
				{name: "fallback uses wallet price and rejects", source: "wallet", preference: "subscription_first", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, wallet: 0.9, prompt: 105000, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, walletBalance: 60000, subBalance: 50000, reject: true},
				{name: "fallback uses cheaper wallet price", source: "wallet", preference: "subscription_first", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.9, wallet: 0.5, prompt: 105000, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, walletBalance: 60000, subBalance: 90000, wantHeld: 52500, wantDebit: 16500, wantList: 33000},
				{name: "rounded zero subscription holds and returns one", source: "subscription", expression: `tier("base", p * 0.2)`, subscription: 0.5, wallet: 1, prompt: 1, usage: dto.Usage{PromptTokens: 1, TotalTokens: 1}, wantHeld: 1},
				{name: "supplement failure retains only paid reservation", source: "subscription", expression: `tier("base", p * 2 + c * 6)`, subscription: 0.5, wallet: 1, prompt: 100, usage: dto.Usage{PromptTokens: 100, CompletionTokens: 1000, TotalTokens: 1100}, wantHeld: 50, wantDebit: 50, wantList: 3100, subBalance: 100, wantDue: 1550},
				{name: "group retry reserves adjusted amount", source: "subscription", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, wallet: 0.9, prompt: 105000, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, wantHeld: 105000, wantDebit: 33000, wantList: 66000, retryGroup: true},
				{name: "period reset cannot refund new allowance", source: "subscription", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, wallet: 0.9, prompt: 105000, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, wantHeld: 52500, wantDebit: 52500, wantList: 33000, wantDue: 16500, periodChanged: true},
				{name: "HTTP router subscription", source: "subscription", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, wallet: 0.9, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, wantDebit: 16500, wantList: 33000, httpRelay: true},
				{name: "SSE router wallet", source: "wallet", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, wallet: 0.9, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, wantDebit: 29700, wantList: 33000, httpRelay: true, stream: true},
				{name: "SSE router safe output fallback", source: "wallet", expression: `tier("output", output_before(256000) * 6 + (c - output_before(256000)) * 12)`, subscription: 0.5, wallet: 0.9, usage: dto.Usage{PromptTokens: 255000, CompletionTokens: 3000, TotalTokens: 258000}, wantDebit: 8100, wantList: 9000, httpRelay: true, stream: true},
				{name: "trusted wallet still pays final price", source: "wallet", expression: `tier("standard", p * 2 + cr * 0.5 + c * 6)`, subscription: 0.5, wallet: 0.9, prompt: 105000, usage: dto.Usage{PromptTokens: 105000, CompletionTokens: 1000, TotalTokens: 106000, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 100000}}, wantHeld: 0, wantDebit: 29700, wantList: 33000, trusted: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					name := fmt.Sprintf("funding-contract-%s-%d", dialect.kind, i)
					if tc.audioResponse {
						name = "gpt-4o-audio-" + name
					}
					snapshot, err := model.GetModelPricingSnapshot([]string{name})
					require.NoError(t, err)
					pricing := model.PricingValues{
						"billing_setting.billing_mode":            "tiered_expr",
						"billing_setting.billing_expr":            tc.expression,
						"billing_setting.subscription_multiplier": tc.subscription,
					}
					if tc.audio {
						pricing["AudioRatio"] = float64(1)
					}
					if tc.wallet > 0 {
						pricing["billing_setting.wallet_multiplier"] = tc.wallet
					}
					if i == 0 {
						for _, key := range []string{"billing_setting.subscription_multiplier", "billing_setting.wallet_multiplier"} {
							for _, value := range []string{"0", "-0.5", "1e999"} {
								body, marshalErr := common.Marshal(OptionUpdateRequest{Key: key, Value: fmt.Sprintf(`{"%s":%s}`, name, value)})
								require.NoError(t, marshalErr)
								recorder := httptest.NewRecorder()
								context, _ := gin.CreateTestContext(recorder)
								context.Request = httptest.NewRequest(http.MethodPut, "/api/option/", strings.NewReader(string(body)))
								UpdateOption(context)
								assert.Contains(t, recorder.Body.String(), `"success":false`)
							}
						}
					}
					require.NoError(t, model.UpdateModelPricing([]model.ModelPricingChange{{ModelName: name, ExpectedVersion: snapshot.Entries[0].Version, Pricing: pricing}}))
					if i == 0 {
						before, snapshotErr := model.GetModelPricingSnapshot([]string{name})
						require.NoError(t, snapshotErr)
						require.NoError(t, model.UpdateModelPricing([]model.ModelPricingChange{{ModelName: name, ExpectedVersion: before.Entries[0].Version, Pricing: model.PricingValues{"billing_setting.subscription_multiplier": float64(0.75)}}}))
						changed, snapshotErr := model.GetModelPricingSnapshot([]string{name})
						require.NoError(t, snapshotErr)
						assert.NotEqual(t, before.Entries[0].Version, changed.Entries[0].Version, "multiplier-only changes must invalidate stale editor versions")
						require.Error(t, model.UpdateModelPricing([]model.ModelPricingChange{{ModelName: name, ExpectedVersion: before.Entries[0].Version, Pricing: model.PricingValues{"billing_setting.wallet_multiplier": float64(2)}}}))
						require.NoError(t, model.UpdateModelPricing([]model.ModelPricingChange{{ModelName: name, ExpectedVersion: changed.Entries[0].Version, Pricing: pricing}}))
						persisted := map[string]string{}
						for _, key := range []string{"billing_setting.subscription_multiplier", "billing_setting.wallet_multiplier"} {
							var option model.Option
							require.NoError(t, db.Where(map[string]any{"key": key}).First(&option).Error)
							persisted[key] = option.Value
						}
						require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.subscription_multiplier": "{}", "billing_setting.wallet_multiplier": "{}"}))
						require.NoError(t, config.GlobalConfig.LoadFromDB(persisted))
					}

					walletBalance, subBalance := 1000000, 1000000
					if tc.walletBalance > 0 {
						walletBalance = tc.walletBalance
					}
					if tc.subBalance > 0 {
						subBalance = tc.subBalance
					}
					trust := "0"
					if tc.trusted {
						trust = "0.1"
					}
					require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"quota_setting.trust_quota_usd": trust}))
					user := model.User{Username: fmt.Sprintf("funding-user-%d", i), Quota: walletBalance, Group: "default", AffCode: fmt.Sprintf("funding-aff-%d", i)}
					require.NoError(t, db.Create(&user).Error)
					token := model.Token{UserId: user.Id, Key: fmt.Sprintf("fundingtoken%d", i), Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000000}
					require.NoError(t, db.Create(&token).Error)
					channel := model.Channel{Type: 1, Name: name, Key: "test-upstream-key", Models: name, Group: "default", Status: common.ChannelStatusEnabled}
					require.NoError(t, db.Create(&channel).Error)
					if tc.audio {
						require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Model: name, Group: "default", Enabled: true}).Error)
						model.InvalidatePricingCache()
						found := false
						for _, row := range model.GetPricing() {
							if row.ModelName == name {
								found = true
								assert.Nil(t, row.WalletMultiplier)
								assert.Nil(t, row.SubscriptionMultiplier)
							}
						}
						require.True(t, found, "audio model still appears in catalog without an effective text multiplier")
					}
					plan := model.SubscriptionPlan{Title: name, TotalAmount: int64(subBalance), QuotaResetPeriod: "never", AllowWalletOverflow: common.GetPointer(true)}
					require.NoError(t, db.Create(&plan).Error)
					now := common.GetTimestamp()
					subscription := model.UserSubscription{UserId: user.Id, PlanId: plan.Id, AmountTotal: int64(subBalance), StartTime: now, EndTime: now + 3600, Status: "active", Source: "admin", AllowWalletOverflow: true}
					require.NoError(t, db.Create(&subscription).Error)
					ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
					ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
					ctx.Set("token_quota", 1000000)
					ctx.Set("username", user.Username)
					requestID := common.NewRequestId()
					ctx.Set(common.RequestIdKey, requestID)
					info := &relaycommon.RelayInfo{
						UserId: user.Id, TokenId: token.Id, TokenKey: token.Key,
						ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: constant.ChannelTypeOpenAI},
						OriginModelName: name, BillingModelName: name, UserGroup: "default", UsingGroup: "default",
						RelayMode: relayconstant.RelayModeChatCompletions, BillingRequestInput: &billingexpr.RequestInput{},
						StartTime: time.Now(), RequestId: requestID,
					}
					info.UserSetting.BillingPreference = tc.source + "_only"
					if tc.preference != "" {
						info.UserSetting.BillingPreference = tc.preference
					}
					if tc.httpRelay {
						user.Setting = fmt.Sprintf("{\"billing_preference\":\"%s_only\"}", tc.source)
						require.NoError(t, db.Model(&user).Update("setting", user.Setting).Error)
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							path := "/v1/chat/completions"
							if tc.compact {
								path = "/v1/responses/compact"
							}
							assert.Equal(t, path, r.URL.Path)
							if tc.changeConfig {
								current, snapshotErr := model.GetModelPricingSnapshot([]string{name})
								if !assert.NoError(t, snapshotErr) {
									return
								}
								changed := model.PricingValues{"billing_setting.billing_mode": "tiered_expr", "billing_setting.billing_expr": `tier("changed", p * 9)`, "billing_setting.wallet_multiplier": float64(1.5)}
								if !assert.NoError(t, model.UpdateModelPricing([]model.ModelPricingChange{{ModelName: name, ExpectedVersion: current.Entries[0].Version, Pricing: changed}})) {
									return
								}
							}
							payload := map[string]any{"id": "fixture", "object": "chat.completion", "model": name, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "answer"}, "finish_reason": "stop"}}, "usage": tc.usage}
							if tc.compact {
								payload = map[string]any{"id": "fixture", "object": "response.compaction", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}}}, "usage": map[string]any{"input_tokens": tc.usage.PromptTokens, "output_tokens": tc.usage.CompletionTokens, "total_tokens": tc.usage.TotalTokens}}
							}
							if tc.stream {
								w.Header().Set("Content-Type", "text/event-stream")
								payload["object"] = "chat.completion.chunk"
								payload["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "answer"}, "finish_reason": "stop"}}
								encoded, encodeErr := common.Marshal(payload)
								if assert.NoError(t, encodeErr) {
									_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", encoded)
								}
								return
							}
							w.Header().Set("Content-Type", "application/json")
							encoded, encodeErr := common.Marshal(payload)
							if assert.NoError(t, encodeErr) {
								_, _ = w.Write(encoded)
							}
						}))
						defer upstream.Close()
						channel.BaseURL = &upstream.URL
						channel.Type, channel.Status, channel.Models, channel.Group = constant.ChannelTypeOpenAI, common.ChannelStatusEnabled, name, "default"
						require.NoError(t, db.Save(&channel).Error)
						require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Model: name, Group: "default", Enabled: true}).Error)
						engine := gin.New()
						engine.Use(middleware.BodyStorageCleanup())
						path := "/v1/chat/completions"
						relayFormat := types.RelayFormatOpenAI
						if tc.compact {
							path = "/v1/responses/compact"
							relayFormat = types.RelayFormatOpenAIResponsesCompaction
						}
						engine.POST(path, middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) {
							c.Set(common.RequestIdKey, requestID)
							Relay(c, relayFormat)
						})
						body := map[string]any{"model": name, "messages": []any{map[string]any{"role": "user", "content": "test"}}, "stream": tc.stream, "stream_options": map[string]any{"include_usage": true}}
						if tc.compact {
							body = map[string]any{"model": name, "input": "test"}
						}
						requestBody, bodyErr := common.Marshal(body)
						require.NoError(t, bodyErr)
						request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(requestBody)))
						request.Header.Set("Authorization", "Bearer sk-"+token.Key)
						request.Header.Set("Content-Type", "application/json")
						recorder := httptest.NewRecorder()
						engine.ServeHTTP(recorder, request)
						require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
						assert.Contains(t, recorder.Body.String(), "answer")
					} else {
						if tc.audio {
							info.Request = &dto.GeneralOpenAIRequest{Model: name, Modalities: []byte(`["text","audio"]`)}
							if tc.audioResponse {
								info.RelayMode = relayconstant.RelayModeResponses
								info.Request = &dto.OpenAIResponsesRequest{Model: name, Input: []byte(`"test"`)}
							}
						}
						if tc.geminiAudio != "" {
							info.RelayMode = relayconstant.RelayModeGemini
							request := &dto.GeminiChatRequest{}
							payload := `{"contents":[{"parts":[{"inline_data":{"mime_type":"audio/wav","data":"AA=="}}]}]}`
							if tc.geminiAudio == "file" {
								payload = `{"contents":[{"parts":[{"fileData":{"mimeType":"Audio/Wav","fileUri":"gs://fixture/audio"}}]}]}`
							}
							if tc.geminiAudio == "output" {
								payload = `{"contents":[{"parts":[{"text":"test"}]}],"generationConfig":{"response_modalities":["AUDIO"]}}`
							}
							require.NoError(t, common.UnmarshalJsonStr(payload, request))
							info.Request = request
						}
						price, err := helper.ModelPriceHelper(ctx, info, tc.prompt, &types.TokenCountMeta{})
						require.NoError(t, err)
						preErr := service.PreConsumeBilling(ctx, price.QuotaToPreConsume, info)
						if tc.reject {
							require.NotNil(t, preErr)
							assert.Equal(t, types.ErrorCodeInsufficientUserQuota, preErr.GetErrorCode())
							require.NoError(t, db.First(&user, user.Id).Error)
							require.NoError(t, db.First(&token, token.Id).Error)
							require.NoError(t, db.First(&subscription, subscription.Id).Error)
							assert.Equal(t, walletBalance, user.Quota)
							assert.Zero(t, subscription.AmountUsed)
							assert.Equal(t, 1000000, token.RemainQuota)
							return
						}
						require.Nil(t, preErr)
						if tc.resetBeforeRetry {
							require.NoError(t, db.Model(&subscription).Updates(map[string]any{"last_reset_time": int64(12345), "amount_used": int64(123)}).Error)
							require.Error(t, info.Billing.Reserve(100))
						}
						if tc.retryGroup {
							require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"group_ratio_setting.group_ratio": `{"default":1,"retry":2}`}))
							info.UsingGroup = "retry"
							info.PriceData.GroupRatioInfo.GroupRatio = 2
							require.Nil(t, service.PrepareTieredBillingForSelectedGroup(ctx, info))
						}
						assert.Equal(t, tc.source, info.BillingSource)
						if !tc.resetBeforeRetry {
							assert.Equal(t, tc.wantHeld, info.FinalPreConsumedQuota)
						}

						if tc.changeConfig {
							current, snapshotErr := model.GetModelPricingSnapshot([]string{name})
							require.NoError(t, snapshotErr)
							pricing["billing_setting.subscription_multiplier"] = float64(1.5)
							pricing["billing_setting.billing_expr"] = `tier("changed", p * 9 + c * 18)`
							require.NoError(t, model.UpdateModelPricing([]model.ModelPricingChange{{ModelName: name, ExpectedVersion: current.Entries[0].Version, Pricing: pricing}}))
						}

						if tc.periodChanged {
							require.NoError(t, db.Model(&subscription).Updates(map[string]any{"last_reset_time": int64(12345), "amount_used": int64(123)}).Error)
						}
						if tc.refund {
							info.Billing.Refund(ctx)
							require.Eventually(t, func() bool {
								var sub model.UserSubscription
								var key model.Token
								var record model.SubscriptionPreConsumeRecord
								if db.First(&sub, subscription.Id).Error != nil || db.First(&key, token.Id).Error != nil || db.Where("request_id = ?", info.RequestId).First(&record).Error != nil {
									return false
								}
								return sub.AmountUsed == 123 && key.RemainQuota == 1000000 && record.Status == "refunded"
							}, 5*time.Second, 10*time.Millisecond, "refund must leave new period untouched")
							require.NoError(t, db.First(&user, user.Id).Error)
							assert.Zero(t, user.UsedQuota)
							return
						}
						if tc.settlementFailure != "" {
							callback := "funding-settlement-failure"
							require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
								updates, ok := tx.Statement.Dest.(map[string]any)
								if !ok {
									return
								}
								field := ""
								if tc.settlementFailure == "wallet" && tx.Statement.Table == "users" {
									field = "quota"
								}
								if tc.settlementFailure == "token" && tx.Statement.Table == "tokens" {
									field = "remain_quota"
								}
								adjustment, ok := updates[field].(clause.Expr)
								if ok && (adjustment.SQL == field+" - ?" || tc.settlementFailure == "token" && adjustment.SQL == field+" + ?") {
									tx.AddError(fmt.Errorf("controlled %s settlement failure", tc.settlementFailure))
								}
							}))
							t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
						}
						if tc.deleteToken {
							require.NoError(t, token.Delete())
						}
						if tc.toolFailure {
							ctx.Set("claude_web_search_requests", 1)
						}
						if tc.audio || tc.geminiAudio != "" {
							assert.Nil(t, info.FundingPricing, "audio must not acquire a text funding policy")
						}
						if tc.audio {
							service.PostAudioConsumeQuota(ctx, info, &tc.usage, "")
						} else {
							service.PostTextConsumeQuota(ctx, info, &tc.usage, nil)
						}
						if tc.wantDue == 0 {
							require.NoError(t, info.Billing.Settle(tc.wantDebit), "duplicate settlement must not charge again")
						}
					}
					require.NoError(t, db.First(&user, user.Id).Error)
					require.NoError(t, db.Unscoped().First(&token, token.Id).Error)
					require.NoError(t, db.First(&channel, channel.Id).Error)
					require.NoError(t, db.First(&subscription, subscription.Id).Error)
					wantWallet, wantSubscription := walletBalance, int64(0)
					if tc.source == "subscription" {
						wantSubscription = int64(tc.wantDebit)
					} else {
						wantWallet -= tc.wantDebit
					}
					if tc.periodChanged {
						wantSubscription = 123
					}
					wantToken := tc.wantDebit
					if tc.settlementFailure == "token" || tc.deleteToken {
						wantToken = tc.wantHeld
					}
					if common.BatchUpdateEnabled || common.RedisEnabled {
						require.Eventually(t, func() bool {
							var paid model.User
							var key model.Token
							var used model.Channel
							if db.First(&paid, user.Id).Error != nil || db.Unscoped().First(&key, token.Id).Error != nil || db.First(&used, channel.Id).Error != nil {
								return false
							}
							if paid.Quota != wantWallet || paid.UsedQuota != tc.wantDebit || paid.RequestCount != 1 || key.UsedQuota != wantToken || used.UsedQuota != int64(tc.wantList) {
								return false
							}
							if common.RedisEnabled {
								balance, err := model.GetUserQuota(user.Id, false)
								if err != nil || balance != wantWallet {
									return false
								}
								cached, err := model.GetTokenByKey(token.Key, false)
								if tc.deleteToken {
									if !errors.Is(err, gorm.ErrRecordNotFound) {
										return false
									}
								} else if err != nil || cached.RemainQuota != 1000000-wantToken {
									return false
								}
							}
							return true
						}, 5*time.Second, 10*time.Millisecond, "wait for the real Redis cache and production batch updater to commit")
					}
					require.NoError(t, db.First(&user, user.Id).Error)
					require.NoError(t, db.Unscoped().First(&token, token.Id).Error)
					require.NoError(t, db.First(&channel, channel.Id).Error)
					require.NoError(t, db.First(&subscription, subscription.Id).Error)
					assert.Equal(t, wantWallet, user.Quota)
					assert.Equal(t, wantSubscription, subscription.AmountUsed)
					assert.Equal(t, 1000000-wantToken, token.RemainQuota)
					assert.Equal(t, wantToken, token.UsedQuota)
					assert.Equal(t, tc.wantDebit, user.UsedQuota)
					assert.Equal(t, 1, user.RequestCount)
					assert.Equal(t, int64(tc.wantList), channel.UsedQuota)
					var logs []model.Log
					require.NoError(t, model.LOG_DB.Where("request_id = ? AND user_id = ? AND model_name = ?", requestID, user.Id, name).Find(&logs).Error)
					require.Len(t, logs, 1)
					assert.Equal(t, tc.wantDebit, logs[0].Quota)
					assert.Equal(t, tc.usage.PromptTokens, logs[0].PromptTokens)
					assert.Equal(t, tc.usage.CompletionTokens, logs[0].CompletionTokens)
					var other map[string]any
					require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
					if tc.audio || tc.geminiAudio != "" {
						assert.NotContains(t, other, "funding_pricing")
						return
					}
					funding, ok := other["funding_pricing"].(map[string]any)
					require.True(t, ok, "new logs must explain list price and debit")
					assert.Equal(t, float64(tc.wantList), funding["list_quota"])
					assert.Equal(t, float64(tc.wantDebit), funding["charged_quota"])
					if tc.settlementFailure == "token" || tc.deleteToken {
						assert.Equal(t, "failed", funding["token_status"])
					}
					wantDue, wantStatus := tc.wantDebit, "settled"
					if tc.wantDue > 0 {
						wantDue, wantStatus = tc.wantDue, "partial"
					}
					assert.Equal(t, float64(wantDue), funding["due_quota"])
					assert.Equal(t, wantStatus, funding["funding_status"])
					assert.Equal(t, tc.source, funding["source"])
					assert.Equal(t, name, funding["model_name"])
				})
			}
		})
	}
}
