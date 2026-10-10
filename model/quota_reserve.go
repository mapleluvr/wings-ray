package model

import (
	"context"
	"errors"
	"fmt"
	"github.com/bytedance/gopkg/util/gopool"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

var ErrQuotaCachePending = errors.New("quota cache reconciliation is pending")

type cacheQuotaResult int

const (
	cacheQuotaInsufficient cacheQuotaResult = iota
	cacheQuotaOK
	cacheQuotaMiss
)

const userQuotaReserveScript = `
if tonumber(redis.call('HGET', KEYS[2], 'pending') or '0') > 0 then return -2 end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[3])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
local quota = tonumber(redis.call('HGET', KEYS[1], 'Quota'))
if quota == nil or quota < tonumber(ARGV[1]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'Quota', -tonumber(ARGV[1]))
return 1`

const userQuotaDeltaScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[3])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'Quota', ARGV[1])
return 1`

const tokenQuotaReserveScript = `
if tonumber(redis.call('HGET', KEYS[2], 'pending') or '0') > 0 then return -2 end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
local remain = tonumber(redis.call('HGET', KEYS[1], 'RemainQuota'))
if remain == nil or remain < tonumber(ARGV[1]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', -tonumber(ARGV[1]))
redis.call('HINCRBY', KEYS[1], 'UsedQuota', tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[3])
return 1`

const tokenQuotaDeltaScript = `
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', tonumber(ARGV[1]))
redis.call('HINCRBY', KEYS[1], 'UsedQuota', -tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[3])
return 1`

func quotaCacheGeneration(cacheKey string) (int64, error) {
	if !common.RedisEnabled {
		return 0, nil
	}
	generation, err := common.RDB.HGet(context.Background(), cacheKey+":quota-fence", "generation").Int64()
	if err == redis.Nil {
		return 0, nil
	}
	return generation, err
}

// commitQuotaCacheDelta fences cold snapshots across a direct SQL write. Live
// hashes keep outstanding batch reservations; only that existing hash receives
// the committed delta. A cold snapshot may publish only outside pending writes
// and against the generation captured before its database read.
func commitQuotaCacheDelta(cacheKey string, id, delta int, token bool, write func() error) error {
	if !common.RedisEnabled {
		return write()
	}
	client := common.RDB
	operation := cacheKey + ":quota-write:" + common.GetUUID()
	const begin = `
local previous = redis.call('GET', KEYS[3])
if previous then return tonumber(previous) end
local present = redis.call('EXISTS', KEYS[1])
redis.call('SET', KEYS[3], present)
redis.call('HINCRBY', KEYS[2], 'generation', 1)
redis.call('HINCRBY', KEYS[2], 'pending', 1)
if present == 1 then redis.call('PERSIST', KEYS[1]) end
return present`
	present, err := client.Eval(context.Background(), begin, []string{cacheKey, cacheKey + ":quota-fence", operation}).Int()
	if err != nil {
		// The begin reply may be lost. Abort only the cache operation, never SQL.
		reconcileQuotaCacheDelta(client, cacheKey, operation, id, delta, false, token)
		return err
	}
	writeErr := write()
	reconcileQuotaCacheDelta(client, cacheKey, operation, id, delta, present == 1 && writeErr == nil, token)
	return writeErr
}

// Completion is idempotent even if Redis applied it but its reply was lost.
// Keep live batch reservations and block new reservations until reconciliation;
// retries touch Redis only and retain the captured client across shutdown.
func reconcileQuotaCacheDelta(client *redis.Client, cacheKey, operation string, id, delta int, applied, token bool) {
	const finish = `
if redis.call('EXISTS', KEYS[3]) == 0 then return 1 end
if redis.call('GET', KEYS[3]) == '1' then
  if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
    or (ARGV[4] == '1' and (redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0 or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0))
    or (ARGV[4] == '0' and redis.call('HEXISTS', KEYS[1], 'Quota') == 0) then
    return redis.error_reply('quota cache live balance missing during reconciliation')
  end
end
if ARGV[1] == '1' and tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') == tonumber(ARGV[2]) then
  if ARGV[4] == '1' then
    if redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 1 and redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 1 then
      redis.call('HINCRBY', KEYS[1], 'RemainQuota', ARGV[3])
      redis.call('HINCRBY', KEYS[1], 'UsedQuota', -tonumber(ARGV[3]))
      redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[5])
    end
  elseif redis.call('HEXISTS', KEYS[1], 'Quota') == 1 then
    redis.call('HINCRBY', KEYS[1], 'Quota', ARGV[3])
  end
end
redis.call('HINCRBY', KEYS[2], 'generation', 1)
local pending = redis.call('HINCRBY', KEYS[2], 'pending', -1)
redis.call('DEL', KEYS[3])
if pending == 0 then redis.call('EXPIRE', KEYS[1], ARGV[6]) end
return 1`
	applyArg, tokenArg := "0", "0"
	if applied {
		applyArg = "1"
	}
	if token {
		tokenArg = "1"
	}
	keys := []string{cacheKey, cacheKey + ":quota-fence", operation}
	args := []any{applyArg, id, delta, tokenArg, common.GetTimestamp(), userCacheTTLSeconds()}
	complete := func() error { return client.Eval(context.Background(), finish, keys, args...).Err() }
	err := complete()
	if err == nil {
		return
	}
	common.SysError("quota cache reconciliation pending: " + err.Error())
	if errors.Is(err, redis.ErrClosed) {
		return
	}
	gopool.Go(func() {
		for {
			time.Sleep(time.Second)
			retryErr := complete()
			if retryErr == nil || errors.Is(retryErr, redis.ErrClosed) {
				return
			}
		}
	})
}

func checkQuotaCacheFence(cacheKey string) error {
	pending, err := common.RDB.HGet(context.Background(), cacheKey+":quota-fence", "pending").Int64()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		return err
	}
	if pending > 0 {
		return ErrQuotaCachePending
	}
	return nil
}

func quotaResultFromLua(result int, err error) (cacheQuotaResult, error) {
	if err != nil {
		return cacheQuotaMiss, err
	}
	switch result {
	case -2:
		return cacheQuotaMiss, ErrQuotaCachePending
	case 1:
		return cacheQuotaOK, nil
	case 0:
		return cacheQuotaInsufficient, nil
	default:
		return cacheQuotaMiss, nil
	}
}

func cacheTryReserveUserQuota(userID int, amount int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), userQuotaReserveScript,
		[]string{getUserCacheKey(userID), getUserCacheKey(userID) + ":quota-fence"}, amount, userID, userCacheSchemaVersion).Int()
	return quotaResultFromLua(result, err)
}

func cacheApplyUserQuotaDelta(userID int, delta int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), userQuotaDeltaScript,
		[]string{getUserCacheKey(userID)}, delta, userID, userCacheSchemaVersion).Int()
	return quotaResultFromLua(result, err)
}

func cacheTryReserveTokenQuota(id int, key string, amount int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), tokenQuotaReserveScript,
		[]string{getTokenCacheKey(key), getTokenCacheKey(key) + ":quota-fence"}, amount, id, common.GetTimestamp()).Int()
	return quotaResultFromLua(result, err)
}

func cacheApplyTokenQuotaDelta(id int, key string, delta int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), tokenQuotaDeltaScript,
		[]string{getTokenCacheKey(key)}, delta, id, common.GetTimestamp()).Int()
	return quotaResultFromLua(result, err)
}

// persistUserQuotaDelta 把已在缓存侧预扣成功的增量落库；批量模式下入队，
// 直写模式下要求行存在（用户已删除时报错，交由调用方补偿缓存）。
func persistUserQuotaDelta(id int, delta int, immediate ...bool) error {
	if common.BatchUpdateEnabled && (len(immediate) == 0 || !immediate[0]) {
		addNewRecord(BatchUpdateTypeUserQuota, id, delta)
		return nil
	}
	result := DB.Model(&User{}).Where("id = ?", id).Update("quota", gorm.Expr("quota + ?", delta))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func persistTokenQuotaDelta(id int, delta int, immediate ...bool) error {
	if common.BatchUpdateEnabled && (len(immediate) == 0 || !immediate[0]) {
		addNewRecord(BatchUpdateTypeTokenQuota, id, delta)
		return nil
	}
	result := DB.Model(&Token{}).Where("id = ?", id).Updates(
		map[string]any{
			"remain_quota":  gorm.Expr("remain_quota + ?", delta),
			"used_quota":    gorm.Expr("used_quota - ?", delta),
			"accessed_time": common.GetTimestamp(),
		},
	)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func reserveUserQuotaDB(id int, quota int) (bool, error) {
	result := DB.Model(&User{}).
		Where("id = ? AND quota >= ?", id, quota).
		Update("quota", gorm.Expr("quota - ?", quota))
	return result.RowsAffected == 1, result.Error
}

func reserveTokenQuotaDB(id int, quota int) (bool, error) {
	result := DB.Model(&Token{}).
		Where("id = ? AND remain_quota >= ?", id, quota).
		Updates(map[string]any{
			"remain_quota":  gorm.Expr("remain_quota - ?", quota),
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"accessed_time": common.GetTimestamp(),
		})
	return result.RowsAffected == 1, result.Error
}

// TryReserveUserQuota atomically checks and deducts a user's wallet quota.
// 缓存命中时以缓存余额为准（避免批量模式下过期的数据库余额放大并发超扣）；
// 未完成栅栏或批量余额不确定时拒绝预留；普通非批量路径保留数据库降级。
func TryReserveUserQuota(id int, quota int, immediate ...bool) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota 不能为负数！")
	}
	if quota == 0 {
		return true, nil
	}
	if !common.RedisEnabled {
		return reserveUserQuotaDB(id, quota)
	}

	result, err := cacheTryReserveUserQuota(id, int64(quota))
	if err == nil && result == cacheQuotaMiss {
		if _, hydrateErr := GetUserCache(id); hydrateErr == nil {
			result, err = cacheTryReserveUserQuota(id, int64(quota))
		}
	}
	if err != nil || result == cacheQuotaMiss {
		if errors.Is(err, ErrQuotaCachePending) {
			return false, err
		}
		if common.BatchUpdateEnabled {
			if err != nil {
				return false, err
			}
			return false, ErrQuotaCachePending
		}
		fenceErr := checkQuotaCacheFence(getUserCacheKey(id))
		if errors.Is(fenceErr, ErrQuotaCachePending) || fenceErr != nil && len(immediate) > 0 && immediate[0] {
			return false, fenceErr
		}
		if err != nil {
			common.SysLog("user quota cache reserve unavailable, falling back to database: " + err.Error())
		}
		return reserveUserQuotaDB(id, quota)
	}
	if result == cacheQuotaInsufficient {
		return false, nil
	}
	if err = persistUserQuotaDelta(id, -quota, immediate...); err != nil {
		compensated, compensateErr := cacheApplyUserQuotaDelta(id, int64(quota))
		if compensateErr != nil || compensated != cacheQuotaOK {
			common.SysError(fmt.Sprintf("failed to compensate reserved user quota: result=%d error=%v", compensated, compensateErr))
		}
		return false, err
	}
	return true, nil
}

// TryReserveTokenQuota atomically checks and deducts a token quota. Unlimited
// tokens skip the balance check but still update remain/used accounting.
func TryReserveTokenQuota(id int, key string, quota int, unlimited bool, immediate ...bool) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota 不能为负数！")
	}
	if quota == 0 {
		return true, nil
	}
	if unlimited {
		return true, DecreaseTokenQuota(id, key, quota, immediate...)
	}
	if !common.RedisEnabled {
		return reserveTokenQuotaDB(id, quota)
	}

	result, err := cacheTryReserveTokenQuota(id, key, int64(quota))
	if err == nil && result == cacheQuotaMiss {
		if _, hydrateErr := GetTokenByKey(key, true); hydrateErr == nil {
			result, err = cacheTryReserveTokenQuota(id, key, int64(quota))
		}
	}
	if err != nil || result == cacheQuotaMiss {
		if errors.Is(err, ErrQuotaCachePending) {
			return false, err
		}
		if common.BatchUpdateEnabled {
			if err != nil {
				return false, err
			}
			return false, ErrQuotaCachePending
		}
		fenceErr := checkQuotaCacheFence(getTokenCacheKey(key))
		if errors.Is(fenceErr, ErrQuotaCachePending) || fenceErr != nil && len(immediate) > 0 && immediate[0] {
			return false, fenceErr
		}
		if err != nil {
			common.SysLog("token quota cache reserve unavailable, falling back to database: " + err.Error())
		}
		return reserveTokenQuotaDB(id, quota)
	}
	if result == cacheQuotaInsufficient {
		return false, nil
	}
	if err = persistTokenQuotaDelta(id, -quota, immediate...); err != nil {
		compensated, compensateErr := cacheApplyTokenQuotaDelta(id, key, int64(quota))
		if compensateErr != nil || compensated != cacheQuotaOK {
			common.SysError(fmt.Sprintf("failed to compensate reserved token quota: result=%d error=%v", compensated, compensateErr))
		}
		return false, err
	}
	return true, nil
}
