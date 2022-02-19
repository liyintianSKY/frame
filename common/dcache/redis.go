package dcache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hqbobo/frame/common/log"
	"github.com/hqbobo/frame/common/utils"

	"github.com/go-redis/redis/v8"
)

const (
	redis_item_timeout = 60 * 60
	redis_sync_chan    = "dcach_sync"
	redis_sync_set     = 1
	redis_sync_del     = 2
)

type publisher struct {
	From string
	Act  int
	Key  string
	Val  string
	Ttl  int
}

type RedisSession struct {
	client     *redis.Client
	clusterCLi *redis.ClusterClient
	ip         string
	pass       string
	name       string
	mem        *MemSession
	cluster    bool
}

func newRedis(ip string, pass string) *RedisSession {
	s := new(RedisSession)
	s.ip = ip
	s.pass = pass
	s.name = utils.GetRandomString(16)
	s.client = redis.NewClient(&redis.Options{
		Addr:     ip,
		Password: pass, // no password set
		DB:       0,
	})
	s.cluster = false
	s.mem = newMemSession()
	go s.subscribe()
	return s
}

func newRedisCluster(ip []string, pass string) *RedisSession {
	s := new(RedisSession)
	s.pass = pass
	s.name = utils.GetRandomString(16)
	s.clusterCLi = redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:    ip,
		Password: pass, // no password set
	})
	s.cluster = true
	s.mem = newMemSession()
	go s.subscribe()
	return s
}

var ctx = context.Background()

//监听数据修改事件
func (rs *RedisSession) subscribe() {
	var sub *redis.PubSub
	if rs.cluster {
		sub = rs.clusterCLi.Subscribe(ctx, redis_sync_chan)
	} else {
		sub = rs.client.Subscribe(ctx, redis_sync_chan)
	}
	defer sub.Close()
	var pub publisher
	chn := sub.Channel()
	for msg := range chn {
		if e := json.Unmarshal([]byte(msg.Payload), &pub); e == nil {
			if pub.From != rs.name {
				if pub.From != rs.name {
					if pub.Act == redis_sync_set {
						rs.mem.Set(pub.Key, pub.Val, pub.Ttl)
					} else if pub.Act == redis_sync_del {
						rs.mem.Delete(pub.Key)
					}
				}
			}
		} else {
			log.Warnln(e.Error())
		}
	}
}

//消息推送
func (rs *RedisSession) publish(key, val string, ttl int, act int) {
	p := new(publisher)
	p.Key = key
	p.Val = val
	p.Ttl = ttl
	p.Act = act
	p.From = rs.name

	//转为字符串
	s, e := json.Marshal(p)
	if e != nil {
		log.Warnln(e.Error())
		return
	}
	if rs.cluster {
		rsp := rs.clusterCLi.Publish(ctx, redis_sync_chan, string(s))
		if rsp.Err() != nil {
			log.Warnln(rsp.Err().Error())
		}
	} else {
		rsp := rs.client.Publish(ctx, redis_sync_chan, string(s))
		if rsp.Err() != nil {
			log.Warnln(rsp.Err().Error())
		}
	}
}

//获取超时
func (rs *RedisSession) getTtl(key string) (int, bool) {
	var dur *redis.DurationCmd
	if rs.cluster {
		dur = rs.clusterCLi.TTL(ctx, key)
	} else {
		dur = rs.client.TTL(ctx, key)
	}
	return int(dur.Val().Seconds()), true
}
func (rs *RedisSession) HGet(key, field string) (string, error) {
	if rs.cluster {
		return rs.clusterCLi.HGet(ctx, key, field).Result()
	}
	return rs.client.HGet(ctx, key, field).Result()
}
func (rs *RedisSession) HSet(key, field, data string) error {
	if rs.cluster {
		return rs.clusterCLi.HSet(ctx, key, field, data).Err()
	}
	return rs.client.HSet(ctx, key, field, data).Err()
}

func (rs *RedisSession) HDel(key, field string) error {
	if rs.cluster {
		return rs.clusterCLi.HDel(ctx, key).Err()
	}
	return rs.client.HDel(ctx, key).Err()
}

func (rs *RedisSession) Get(key string, data interface{}) bool {
	var s string
	if !rs.mem.Get(key, &s) {
		var str *redis.StringCmd
		if rs.cluster {
			str = rs.clusterCLi.Get(ctx, key)
		} else {
			str = rs.client.Get(ctx, key)
		}
		if str.Err() != nil {
			log.Warnf("获取key %s 失败, %s", key, str.Err().Error())
			return false
		}
		s = str.Val()

		if ttl, ok := rs.getTtl(key); ok {
			// log.Debugf("load: %s ttl[ %d ] from redis:", str.Val(), ttl)
			if e := json.Unmarshal([]byte(str.Val()), data); e != nil {
				log.Warnln(e.Error())
				return false
			}
			//内存提前5秒超时
			return rs.mem.Set(key, s, ttl-5)
		}
		return false
	}
	if e := json.Unmarshal([]byte(s), data); e != nil {
		log.Warnln("%s - %s ", s, e.Error())
		return false
	}
	return true
}

func (rs *RedisSession) Set(key string, data interface{}, ttl int) bool {
	var rsp *redis.StatusCmd
	//转为字符串
	s, e := json.Marshal(data)
	if e != nil {
		log.Warnln(e.Error())
		return false
	}
	//必须配置超时
	if ttl <= 0 {
		ttl = redis_item_timeout
	}
	if rs.cluster {
		rsp = rs.clusterCLi.Set(ctx, key, s, time.Second*time.Duration(ttl))
	} else {
		rsp = rs.client.Set(ctx, key, s, time.Second*time.Duration(ttl))
	}
	if rsp.Err() != nil {
		log.Warnln(rsp.Err().Error())
	} else {
		//缓存本地
		if rs.mem != nil {
			rs.mem.Set(key, string(s), ttl)
		}
		//通告修改
		go rs.publish(key, string(s), ttl, redis_sync_set)
		return true
	}
	return false
}

func (rs *RedisSession) Delete(key string) bool {
	var rsp *redis.IntCmd
	if rs.cluster {
		rsp = rs.clusterCLi.Del(ctx, key)
	} else {
		rsp = rs.client.Del(ctx, key)
	}
	if rsp.Err() != nil {
		log.Warn("删除", key, "错误:", rsp.Err().Error())
	}
	//缓存本地
	if rs.mem != nil {
		rs.mem.Delete(key)
	}
	//通告删除
	go rs.publish(key, "", 0, redis_sync_del)
	return true
}

func (rs *RedisSession) ScanDelete(key string) (int, error) {
	var cursor uint64
	var n int
	var err error
	for {
		var keys []string
		//扫描key，每次100条
		if rs.cluster {
			keys, cursor, err = rs.clusterCLi.Scan(ctx, cursor, key, 100).Result()
		} else {
			keys, cursor, err = rs.client.Scan(ctx, cursor, key, 100).Result()
		}
		if err != nil {
			log.Error("scan", key, "错误:", err.Error())
			return 0, err
		}
		n += len(keys)
		for _, v := range keys {
			if rs.cluster {
				_, err = rs.clusterCLi.Del(ctx, v).Result()
			} else {
				_, err = rs.client.Del(ctx, v).Result()
			}
			if err != nil {
				log.Error("删除", key, "错误:", err.Error())
				return n, err
			}
		}
		if cursor == 0 {
			break
		}
	}

	return n, nil
}

func (rs *RedisSession) Incr(key string, data interface{}) bool {
	var rsp *redis.IntCmd
	if rs.cluster {
		rsp = rs.clusterCLi.Incr(ctx, key)
	} else {
		rsp = rs.client.Incr(ctx, key)
	}
	if rsp.Err() != nil {
		log.Warnln("Incr key %s 失败, %s", key, rsp.Err().Error())
		return false
	}
	*data.(*int64) = rsp.Val()
	return true
}

func (rs *RedisSession) IncrBy(key string, data int64) int64 {
	var rsp *redis.IntCmd
	if rs.cluster {
		rsp = rs.clusterCLi.IncrBy(ctx, key, data)
	} else {
		rsp = rs.client.IncrBy(ctx, key, data)
	}
	if rsp.Err() != nil {
		log.Warnf("Incrby key %s 失败, %s", key, rsp.Err().Error())
	}
	return rsp.Val()
}

func (rs *RedisSession) Check(key string) bool {
	var rsp *redis.IntCmd
	if rs.cluster {
		rsp = rs.clusterCLi.Exists(ctx, key)
	} else {
		rsp = rs.client.Exists(ctx, key)
	}
	if rsp.Val() == 1 {
		return true
	}
	return false
}

func (rs *RedisSession) CheckMem(key string) bool {
	return rs.mem.Check(key)
}

//ZADD 添加到有序集合里面 比如存用户就是 zadd user 1 1001
func (rs *RedisSession) ZADD(key string, score float64, member interface{}) {
	data := &redis.Z{
		Score:  score,
		Member: member,
	}
	var rsp *redis.IntCmd
	if rs.cluster {
		rsp = rs.clusterCLi.ZAdd(ctx, key, data)
	} else {
		rsp = rs.client.ZAdd(ctx, key, data)
	}
	if rsp.Err() != nil {
		log.Warnf("ZADD key %s 失败, %s", key, rsp.Err().Error())
	}
}

//Zrange 遍历有序集合
func (rs *RedisSession) Zrange(key string, start, stop int64) []string {
	//data := &redis.StringSliceCmd{}
	var rsp *redis.StringSliceCmd
	if rs.cluster {
		rsp = rs.clusterCLi.ZRange(ctx, key, start, stop)
	} else {
		rsp = rs.client.ZRange(ctx, key, start, stop)
	}
	if rsp.Err() != nil {
		log.Warnln("ZADD key %s 失败, %s", key, rsp.Err().Error())
		return nil
	}
	//fmt.Println("rsp.Val() :::::: ",rsp.Val())
	return rsp.Val()
}

//ZRangeWithScores 遍历有序集合
func (rs *RedisSession) ZRangeWithScores(key string, start, stop int64) []redis.Z {
	//data := &redis.StringSliceCmd{}
	var rsp *redis.ZSliceCmd
	if rs.cluster {
		rsp = rs.clusterCLi.ZRangeWithScores(ctx, key, start, stop)
	} else {
		rsp = rs.client.ZRangeWithScores(ctx, key, start, stop)
	}
	if rsp.Err() != nil {
		log.Warnln("ZADD key %s 失败, %s", key, rsp.Err().Error())
		return nil
	}
	//fmt.Println("rsp.Val() :::::: ",rsp.Val())
	return rsp.Val()
}

//ZRangeWithScores 遍历有序集合
func (rs *RedisSession) ZRevRangeWithScores(key string, start, stop int64) []redis.Z {
	//data := &redis.StringSliceCmd{}
	var rsp *redis.ZSliceCmd
	if rs.cluster {
		rsp = rs.clusterCLi.ZRevRangeWithScores(ctx, key, start, stop)
	} else {
		rsp = rs.client.ZRevRangeWithScores(ctx, key, start, stop)
	}
	if rsp.Err() != nil {
		log.Warnln("ZADD key %s 失败, %s", key, rsp.Err().Error())
		return nil
	}
	//fmt.Println("rsp.Val() :::::: ",rsp.Val())
	return rsp.Val()
}

//ZREM 删除有序集合中某个元素
func (rs *RedisSession) ZREM(key string, member interface{}) {
	//data := &redis.StringSliceCmd{}
	var rsp *redis.IntCmd
	if rs.cluster {
		rsp = rs.clusterCLi.ZRem(ctx, key, member)
	} else {
		rsp = rs.client.ZRem(ctx, key, member)
	}
	if rsp.Err() != nil {
		log.Warnln("Zrem key %s 失败, %s", key, rsp.Err().Error())
		return
	}
	return
}

//Zcard 返回集合数
func (rs *RedisSession) Zcard(key string) int64 {
	//data := &redis.StringSliceCmd{}
	var rsp *redis.IntCmd
	if rs.cluster {
		rsp = rs.clusterCLi.ZCard(ctx, key)
	} else {
		rsp = rs.client.ZCard(ctx, key)
	}
	if rsp.Err() != nil {
		log.Warnln("Zrem key %s 失败, %s", key, rsp.Err().Error())
		return 0
	}
	return rsp.Val()
}

//SetNx 设置nx
/*
* 如果不存在相关的key,value 则设置,否则不设置
* 参数说明:
* @param:key   redis中的key
* @param:value redis中的value
* @param:tm 	redis中的超时
 */
func (rs *RedisSession) SetNx(key string, value interface{}, tm int) (bool, error) {
	if rs.cluster {
		return rs.clusterCLi.SetNX(ctx, key, value, time.Second*time.Duration(tm)).Result()
	} else {
		return rs.client.SetNX(ctx, key, value, time.Second*time.Duration(tm)).Result()
	}
}
