package billing_setting

import (
	"fmt"
	"maps"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/samber/lo"
)

const (
	BillingModeRatio             = "ratio"
	BillingModeTieredExpr        = "tiered_expr"
	BillingModeField             = "billing_mode"
	BillingExprField             = "billing_expr"
	PluginBillingExprOption      = "billing_setting.plugin_billing_expr"
	SubscriptionMultiplierOption = "billing_setting.subscription_multiplier"
	WalletMultiplierOption       = "billing_setting.wallet_multiplier"
	maxTaskExprSmokeTests        = 64
)

// BillingSetting is managed by config.GlobalConfig.Register.
// DB keys: billing_setting.billing_mode, billing_setting.billing_expr,
// billing_setting.plugin_billing_expr
type BillingSetting struct {
	BillingMode            map[string]string  `json:"billing_mode"`
	BillingExpr            map[string]string  `json:"billing_expr"`
	PluginBillingExpr      map[string]string  `json:"plugin_billing_expr"`
	SubscriptionMultiplier map[string]float64 `json:"subscription_multiplier"`
	WalletMultiplier       map[string]float64 `json:"wallet_multiplier"`
}

var billingSetting = BillingSetting{
	BillingMode:            make(map[string]string),
	BillingExpr:            make(map[string]string),
	PluginBillingExpr:      make(map[string]string),
	SubscriptionMultiplier: make(map[string]float64),
	WalletMultiplier:       make(map[string]float64),
}

var billingSettingMu sync.RWMutex

// UpdateConfig publishes a complete billing configuration in one step. Decode
// into new maps first: readers must never combine prices from different saves.
func (s *BillingSetting) UpdateConfig(options map[string]string) error {
	billingSettingMu.Lock()
	defer billingSettingMu.Unlock()
	next := *s
	for key, target := range map[string]any{
		"billing_mode": &next.BillingMode, "billing_expr": &next.BillingExpr,
		"plugin_billing_expr":     &next.PluginBillingExpr,
		"subscription_multiplier": &next.SubscriptionMultiplier,
		"wallet_multiplier":       &next.WalletMultiplier,
	} {
		raw, exists := options[key]
		if !exists {
			continue
		}
		// Unmarshal merges map values; clear only the local copy before decoding.
		switch key {
		case "billing_mode":
			next.BillingMode = nil
		case "billing_expr":
			next.BillingExpr = nil
		case "plugin_billing_expr":
			next.PluginBillingExpr = nil
		case "subscription_multiplier":
			next.SubscriptionMultiplier = nil
		case "wallet_multiplier":
			next.WalletMultiplier = nil
		}
		if err := common.UnmarshalJsonStr(raw, target); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	for key, entries := range map[string]map[string]float64{
		"subscription_multiplier": next.SubscriptionMultiplier, "wallet_multiplier": next.WalletMultiplier,
	} {
		for name, value := range entries {
			if strings.TrimSpace(name) == "" || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("%s[%s] must be a finite positive multiplier", key, name)
			}
		}
	}
	*s = next
	return nil
}

func (s *BillingSetting) ExportConfig() (map[string]string, error) {
	billingSettingMu.RLock()
	defer billingSettingMu.RUnlock()
	result := make(map[string]string)
	for key, value := range map[string]any{
		"billing_mode": s.BillingMode, "billing_expr": s.BillingExpr, "plugin_billing_expr": s.PluginBillingExpr,
		"subscription_multiplier": s.SubscriptionMultiplier, "wallet_multiplier": s.WalletMultiplier,
	} {
		encoded, err := common.Marshal(value)
		if err != nil {
			return nil, err
		}
		if string(encoded) == "null" {
			encoded = []byte("{}")
		}
		result[key] = string(encoded)
	}
	return result, nil
}

// PublishBillingOptions is used by transactional saves and database reloads.
func PublishBillingOptions(options map[string]string) error {
	values := make(map[string]string)
	for key, value := range options {
		if field, ok := strings.CutPrefix(key, "billing_setting."); ok {
			values[field] = value
		}
	}
	return billingSetting.UpdateConfig(values)
}

// ModelBillingConfig freezes expression and both independent source policies.
type ModelBillingConfig struct {
	Mode                   string
	Expression             string
	SubscriptionMultiplier float64
	WalletMultiplier       float64
}

func GetModelBillingConfig(model string) ModelBillingConfig {
	billingSettingMu.RLock()
	defer billingSettingMu.RUnlock()
	result := ModelBillingConfig{Mode: getBillingMode(model), SubscriptionMultiplier: 1, WalletMultiplier: 1}
	result.Expression, _ = getBillingExpr(model)
	if value, ok := billingSetting.SubscriptionMultiplier[model]; ok {
		result.SubscriptionMultiplier = value
	}
	if value, ok := billingSetting.WalletMultiplier[model]; ok {
		result.WalletMultiplier = value
	}
	return result
}

// IsTextTokenExpression restricts funding policies to the initial token-only
// scope shared by request pricing and the public catalog.
func IsTextTokenExpression(expression string) bool {
	if billingexpr.UsesFixedPricing(expression) || len(billingexpr.UsedUsageKeys(expression)) != 0 {
		return false
	}
	vars := billingexpr.UsedVars(expression)
	for _, name := range []string{"ai", "ao", "img", "img_o", "img_cr", "image_count"} {
		if vars[name] {
			return false
		}
	}
	return vars != nil
}

func init() {
	config.GlobalConfig.Register("billing_setting", &billingSetting)
}

// ---------------------------------------------------------------------------
// Read accessors (hot path, must be fast)
// ---------------------------------------------------------------------------

func GetBillingMode(model string) string {
	billingSettingMu.RLock()
	defer billingSettingMu.RUnlock()
	return getBillingMode(model)
}

func getBillingMode(model string) string {
	if mode, ok := billingSetting.BillingMode[model]; ok {
		return mode
	}
	if _, ok := builtinBillingExpr[model]; ok {
		// Existing administrator-configured legacy prices take precedence over
		// a newly introduced built-in expression unless a mode was explicit.
		if ratio_setting.HasConfiguredModelRatio(model) {
			return BillingModeRatio
		}
		if _, configured := ratio_setting.GetModelPrice(model, false); configured {
			return BillingModeRatio
		}
		return BillingModeTieredExpr
	}
	return BillingModeRatio
}

func GetBillingExpr(model string) (string, bool) {
	billingSettingMu.RLock()
	defer billingSettingMu.RUnlock()
	return getBillingExpr(model)
}

func getBillingExpr(model string) (string, bool) {
	if expr, ok := billingSetting.BillingExpr[model]; ok {
		return expr, true
	}
	if getBillingMode(model) == BillingModeTieredExpr {
		expr, ok := builtinBillingExpr[model]
		return expr, ok
	}
	return "", false
}

func GetBuiltinBillingExpr(model string) (string, bool) {
	expression, ok := builtinBillingExpr[model]
	return expression, ok
}

func PluginBillingExprKey(pluginKey, model string) string {
	return pluginKey + "::" + model
}

func SplitPluginBillingExprKey(key string) (plugin, model string, ok bool) {
	plugin, model, ok = strings.Cut(key, "::")
	if !ok || !jsplugin.ValidPluginKey(plugin) || strings.TrimSpace(model) == "" {
		return "", "", false
	}
	return plugin, model, true
}

func GetPluginBillingExprCopy() map[string]string {
	billingSettingMu.RLock()
	defer billingSettingMu.RUnlock()
	return maps.Clone(billingSetting.PluginBillingExpr)
}

func GetPluginBillingExpr(pluginKey, model string) (string, bool) {
	billingSettingMu.RLock()
	defer billingSettingMu.RUnlock()
	expression, ok := billingSetting.PluginBillingExpr[PluginBillingExprKey(pluginKey, model)]
	return expression, ok
}

// ResolveTaskBillingExpr selects the executing plugin's override before the
// model expression, retaining the model alias fallback and explicit modes.
func ResolveTaskBillingExpr(pluginKey, model, mappedModel string) (string, bool) {
	if pluginKey != "" {
		if expr, ok := GetPluginBillingExpr(pluginKey, model); ok {
			return expr, true
		}
		if mappedModel != "" && mappedModel != model {
			if expr, ok := GetPluginBillingExpr(pluginKey, mappedModel); ok {
				return expr, true
			}
		}
	}
	if GetBillingMode(model) == BillingModeTieredExpr {
		return GetBillingExpr(model)
	}
	if mappedModel != "" && mappedModel != model && GetBillingMode(mappedModel) == BillingModeTieredExpr {
		expression, ok := GetBillingExpr(mappedModel)
		return expression, ok && strings.TrimSpace(expression) != ""
	}
	return "", false
}

// TaskExprCompatible checks the schema contract even for usage references in
// branches that the current request would not evaluate.
func TaskExprCompatible(expression string, schema map[string]jsplugin.UsageFieldSchema) bool {
	if strings.TrimSpace(expression) == "" {
		return false
	}
	if _, err := billingexpr.CompileFromCache(expression); err != nil {
		return false
	}
	for key := range billingexpr.UsedUsageKeys(expression) {
		if _, exists := schema[key]; !exists {
			return false
		}
	}
	return !billingexpr.UsesFixedPricing(expression)
}

func GetBuiltinBillingExprCopy() map[string]string {
	return lo.Assign(builtinBillingExpr)
}

func GetBillingModeCopy() map[string]string {
	billingSettingMu.RLock()
	defer billingSettingMu.RUnlock()
	modes := lo.Assign(billingSetting.BillingMode)
	for model := range builtinBillingExpr {
		if _, configured := modes[model]; !configured && getBillingMode(model) == BillingModeTieredExpr {
			modes[model] = BillingModeTieredExpr
		}
	}
	return modes
}

func GetBillingExprCopy() map[string]string {
	billingSettingMu.RLock()
	defer billingSettingMu.RUnlock()
	expressions := lo.Assign(billingSetting.BillingExpr)
	for model := range builtinBillingExpr {
		if _, configured := expressions[model]; configured {
			continue
		}
		if expression, ok := getBillingExpr(model); ok {
			expressions[model] = expression
		}
	}
	return expressions
}

func GetPricingSyncData(base map[string]any) map[string]any {
	extra := make(map[string]any, 2)
	if modes := GetBillingModeCopy(); len(modes) > 0 {
		extra[BillingModeField] = modes
	}
	if exprs := GetBillingExprCopy(); len(exprs) > 0 {
		extra[BillingExprField] = exprs
	}
	return lo.Assign(base, extra)
}

// ---------------------------------------------------------------------------
// Smoke test (called externally for validation before save)
// ---------------------------------------------------------------------------

func SmokeTestExpr(exprStr string) error {
	return smokeTestExpr(exprStr)
}

func smokeTestExpr(exprStr string) error {
	if _, err := billingexpr.CompileFromCache(exprStr); err != nil {
		return err
	}
	usageKeys := billingexpr.UsedUsageKeys(exprStr)
	if len(usageKeys) > 0 {
		sortedKeys := make([]string, 0, len(usageKeys))
		for key := range usageKeys {
			sortedKeys = append(sortedKeys, key)
		}
		sort.Strings(sortedKeys)
		return fmt.Errorf("expression references usage keys %v but the model has no task plugin usage schema", sortedKeys)
	}

	vectors := []billingexpr.TokenParams{
		{P: 0, C: 0, Len: 0},
		{P: 1000, C: 1000, Len: 1000},
		{P: 100000, C: 100000, Len: 100000},
		{P: 1000000, C: 1000000, Len: 1000000},
		{P: 300, C: 100, Len: 1000, CR: 100, Img: 400, ImgCR: 200},
		{P: 800, C: 50, Len: 1000, AI: 200, AO: 50},
		{Len: math.MaxInt32, ImgCR: math.MaxInt32},
	}

	for _, v := range vectors {
		for _, request := range billingExprSmokeRequests() {
			result, _, err := billingexpr.RunExprWithRequest(exprStr, v, request)
			if err != nil {
				return fmt.Errorf("vector {p=%g, c=%g}: run failed: %w", v.P, v.C, err)
			}
			if math.IsNaN(result) || math.IsInf(result, 0) || result < 0 {
				return fmt.Errorf("vector {p=%g, c=%g}: result must be finite and non-negative, got %f", v.P, v.C, result)
			}
		}
	}
	return nil
}

// SmokeTestTaskExpr validates a task usage expression against the usage facts
// declared by its plugin. Literal u() keys must be declared; dynamic calls are
// still exercised by the generated runtime vectors when possible.
func SmokeTestTaskExpr(exprStr string, schema map[string]jsplugin.UsageFieldSchema) error {
	if _, err := billingexpr.CompileFromCache(exprStr); err != nil {
		return err
	}
	if billingexpr.UsesFixedPricing(exprStr) {
		return fmt.Errorf("fixed pricing is not supported for task usage expressions")
	}
	for key := range billingexpr.UsedUsageKeys(exprStr) {
		if _, declared := schema[key]; !declared {
			return fmt.Errorf("usage key %q is not declared by the task plugin", key)
		}
	}

	for _, usage := range taskUsageSmokeVectors(schema) {
		for _, request := range billingExprSmokeRequests() {
			request.Usage = usage
			result, _, err := billingexpr.RunExprWithRequest(exprStr, billingexpr.TokenParams{}, request)
			if err != nil {
				return fmt.Errorf("usage vector %v: run failed: %w", usage, err)
			}
			if math.IsNaN(result) || math.IsInf(result, 0) || result < 0 {
				return fmt.Errorf("usage vector %v: result must be finite and non-negative, got %f", usage, result)
			}
		}
	}
	return nil
}

type usageSmokeDimension struct {
	name   string
	values []any
}

func taskUsageSmokeVectors(schema map[string]jsplugin.UsageFieldSchema) []map[string]any {
	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	sort.Strings(names)

	dimensions := make([]usageSmokeDimension, 0, len(names))
	for _, name := range names {
		field := schema[name]
		if len(field.Enum) > 0 {
			values := make([]any, len(field.Enum))
			for index, value := range field.Enum {
				values[index] = value
			}
			dimensions = append(dimensions, usageSmokeDimension{name: name, values: values})
			continue
		}
		if field.Type == "boolean" {
			dimensions = append(dimensions, usageSmokeDimension{name: name, values: []any{false, true}})
			continue
		}
		limit := relaycommon.MaxTaskDurationSeconds
		if field.Unit == "count" {
			limit = dto.MaxImageN
		}
		if field.Unit == "token" || field.Unit == "credit" {
			limit = common.MaxQuota
		}
		dimensions = append(dimensions, usageSmokeDimension{
			name:   name,
			values: []any{float64(0), float64(1), float64(limit)},
		})
	}

	if usageSmokeCombinationCount(dimensions, maxTaskExprSmokeTests) > maxTaskExprSmokeTests {
		for index := range dimensions {
			field := schema[dimensions[index].name]
			if len(field.Enum) <= 2 {
				continue
			}
			dimensions[index].values = []any{field.Enum[0], field.Enum[len(field.Enum)-1]}
		}
	}

	vectors := make([]map[string]any, 0, maxTaskExprSmokeTests)
	var appendVectors func(int, map[string]any)
	appendVectors = func(index int, current map[string]any) {
		if len(vectors) >= maxTaskExprSmokeTests {
			return
		}
		if index == len(dimensions) {
			vector := make(map[string]any, len(current))
			maps.Copy(vector, current)
			vectors = append(vectors, vector)
			return
		}
		for _, value := range dimensions[index].values {
			current[dimensions[index].name] = value
			appendVectors(index+1, current)
		}
		delete(current, dimensions[index].name)
	}
	appendVectors(0, make(map[string]any, len(dimensions)))

	combinationCount := usageSmokeCombinationCount(dimensions, maxTaskExprSmokeTests)
	if combinationCount > maxTaskExprSmokeTests && len(vectors) > 0 {
		last := make(map[string]any, len(dimensions))
		for _, dimension := range dimensions {
			last[dimension.name] = dimension.values[len(dimension.values)-1]
		}
		vectors[len(vectors)-1] = last
	}
	return vectors
}

func usageSmokeCombinationCount(dimensions []usageSmokeDimension, stopAfter int) int {
	count := 1
	for _, dimension := range dimensions {
		if len(dimension.values) == 0 {
			return 0
		}
		if count > stopAfter/len(dimension.values) {
			return stopAfter + 1
		}
		count *= len(dimension.values)
	}
	return count
}

func billingExprSmokeRequests() []billingexpr.RequestInput {
	return []billingexpr.RequestInput{
		{},
		{
			Headers: map[string]string{
				"anthropic-beta": "fast-mode-2026-02-01",
			},
			Body: []byte(`{"service_tier":"fast","stream_options":{"include_usage":true},"messages":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21]}`),
		},
	}
}
