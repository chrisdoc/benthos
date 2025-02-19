package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/go-redis/redis/v7"

	"github.com/benthosdev/benthos/v4/public/bloblang"
	"github.com/benthosdev/benthos/v4/public/service"
)

func redisProcConfig() *service.ConfigSpec {
	spec := service.NewConfigSpec().
		Stable().
		Summary(`Performs actions against Redis that aren't possible using a ` + "[`cache`](/docs/components/processors/cache)" + ` processor. Actions are
performed for each message and the message contents are replaced with the result. In order to merge the result into the original message compose this processor within a ` + "[`branch` processor](/docs/components/processors/branch)" + `.`).
		Categories("Integration")

	for _, f := range clientFields() {
		spec = spec.Field(f)
	}

	return spec.
		Field(service.NewInterpolatedStringField("command").
			Description("The command to execute.").
			Version("4.3.0").
			Example("scard").
			Example("incrby").
			Example(`${! meta("command") }`).
			Default("")).
		Field(service.NewBloblangField("args_mapping").
			Description("A [Bloblang mapping](/docs/guides/bloblang/about) which should evaluate to an array of values matching in size to the number of arguments required for the specified Redis command.").
			Version("4.3.0").
			Example("root = [ this.key ]").
			Example(`root = [ meta("kafka_key"), this.count ]`).
			Default(``)).
		Field(service.NewStringAnnotatedEnumField("operator", map[string]string{
			"keys":   `Returns an array of strings containing all the keys that match the pattern specified by the ` + "`key` field" + `.`,
			"scard":  `Returns the cardinality of a set, or ` + "`0`" + ` if the key does not exist.`,
			"sadd":   `Adds a new member to a set. Returns ` + "`1`" + ` if the member was added.`,
			"incrby": `Increments the number stored at ` + "`key`" + ` by the message content. If the key does not exist, it is set to ` + "`0`" + ` before performing the operation. Returns the value of ` + "`key`" + ` after the increment.`,
		}).
			Description("The operator to apply.").
			Deprecated().
			Optional()).
		Field(service.NewInterpolatedStringField("key").
			Description("A key to use for the target operator.").
			Deprecated().
			Optional()).
		Field(service.NewIntField("retries").
			Description("The maximum number of retries before abandoning a request.").
			Default(3).
			Advanced()).
		Field(service.NewIntField("retry_period").
			Description("The time to wait before consecutive retry attempts.").
			Default("500ms").
			Advanced()).
		LintRule(`
root = if this.contains("operator") && this.contains("command") {
  [ "only one of 'operator' (old style) or 'command' (new style) fields should be specified" ]
}
`).
		Example("Querying Cardinality",
			`If given payloads containing a metadata field `+"`set_key`"+` it's possible to query and store the cardinality of the set for each message using a `+"[`branch` processor](/docs/components/processors/branch)"+` in order to augment rather than replace the message contents:`,
			`
pipeline:
  processors:
    - branch:
        processors:
          - redis:
              url: TODO
              command: scard
              args_mapping: 'root = [ meta("set_key") ]'
        result_map: 'root.cardinality = this'
`).
		Example("Running Total",
			`If we have JSON data containing number of friends visited during covid 19:

`+"```json"+`
{"name":"ash","month":"feb","year":2019,"friends_visited":10}
{"name":"ash","month":"apr","year":2019,"friends_visited":-2}
{"name":"bob","month":"feb","year":2019,"friends_visited":3}
{"name":"bob","month":"apr","year":2019,"friends_visited":1}
`+"```"+`

We can add a field that contains the running total number of friends visited:

`+"```json"+`
{"name":"ash","month":"feb","year":2019,"friends_visited":10,"total":10}
{"name":"ash","month":"apr","year":2019,"friends_visited":-2,"total":8}
{"name":"bob","month":"feb","year":2019,"friends_visited":3,"total":3}
{"name":"bob","month":"apr","year":2019,"friends_visited":1,"total":4}
`+"```"+`

Using the `+"`incrby`"+` command:`,
			`
pipeline:
  processors:
    - branch:
        processors:
          - redis:
              url: TODO
              command: incrby
              args_mapping: 'root = [ this.name, this.friends_visited ]'
        result_map: 'root.total = this'
`)
}

func init() {
	err := service.RegisterBatchProcessor(
		"redis", redisProcConfig(),
		func(conf *service.ParsedConfig, mgr *service.Resources) (service.BatchProcessor, error) {
			return newRedisProcFromConfig(conf, mgr)
		})
	if err != nil {
		panic(err)
	}
}

//------------------------------------------------------------------------------

type redisProc struct {
	log *service.Logger

	key      *service.InterpolatedString
	operator redisOperator

	command     *service.InterpolatedString
	argsMapping *bloblang.Executor

	client      redis.UniversalClient
	retries     int
	retryPeriod time.Duration
}

func newRedisProcFromConfig(conf *service.ParsedConfig, res *service.Resources) (*redisProc, error) {
	client, err := getClient(conf)
	if err != nil {
		return nil, err
	}

	retries, err := conf.FieldInt("retries")
	if err != nil {
		return nil, err
	}

	retryPeriod, err := conf.FieldDuration("retry_period")
	if err != nil {
		return nil, err
	}

	command, err := conf.FieldInterpolatedString("command")
	if err != nil {
		return nil, err
	}

	var argsMapping *bloblang.Executor
	if testStr, _ := conf.FieldString("args_mapping"); testStr != "" {
		if argsMapping, err = conf.FieldBloblang("args_mapping"); err != nil {
			return nil, err
		}
	}

	r := &redisProc{
		log: res.Logger(),

		command:     command,
		argsMapping: argsMapping,

		retries:     retries,
		retryPeriod: retryPeriod,
		client:      client,
	}

	if conf.Contains("key") {
		if r.key, err = conf.FieldInterpolatedString("key"); err != nil {
			return nil, err
		}
	}

	if conf.Contains("operator") {
		operatorStr, err := conf.FieldString("operator")
		if err != nil {
			return nil, err
		}
		if r.operator, err = getRedisOperator(operatorStr); err != nil {
			return nil, err
		}
	}

	return r, nil
}

type redisOperator func(r *redisProc, key string, part *service.Message) error

func newRedisKeysOperator() redisOperator {
	return func(r *redisProc, key string, part *service.Message) error {
		res, err := r.client.Keys(key).Result()

		for i := 0; i <= r.retries && err != nil; i++ {
			r.log.Errorf("Keys command failed: %v\n", err)
			<-time.After(r.retryPeriod)
			res, err = r.client.Keys(key).Result()
		}
		if err != nil {
			return err
		}

		iRes := make([]interface{}, 0, len(res))
		for _, v := range res {
			iRes = append(iRes, v)
		}
		part.SetStructured(iRes)
		return nil
	}
}

func newRedisSCardOperator() redisOperator {
	return func(r *redisProc, key string, part *service.Message) error {
		res, err := r.client.SCard(key).Result()

		for i := 0; i <= r.retries && err != nil; i++ {
			r.log.Errorf("SCard command failed: %v\n", err)
			<-time.After(r.retryPeriod)
			res, err = r.client.SCard(key).Result()
		}
		if err != nil {
			return err
		}

		part.SetBytes(strconv.AppendInt(nil, res, 10))
		return nil
	}
}

func newRedisSAddOperator() redisOperator {
	return func(r *redisProc, key string, part *service.Message) error {
		mBytes, err := part.AsBytes()
		if err != nil {
			return err
		}

		res, err := r.client.SAdd(key, mBytes).Result()

		for i := 0; i <= r.retries && err != nil; i++ {
			r.log.Errorf("SAdd command failed: %v\n", err)
			<-time.After(r.retryPeriod)
			res, err = r.client.SAdd(key, mBytes).Result()
		}
		if err != nil {
			return err
		}

		part.SetBytes(strconv.AppendInt(nil, res, 10))
		return nil
	}
}

func newRedisIncrByOperator() redisOperator {
	return func(r *redisProc, key string, part *service.Message) error {
		mBytes, err := part.AsBytes()
		if err != nil {
			return err
		}

		valueInt, err := strconv.Atoi(string(mBytes))
		if err != nil {
			return err
		}
		res, err := r.client.IncrBy(key, int64(valueInt)).Result()

		for i := 0; i <= r.retries && err != nil; i++ {
			r.log.Errorf("incrby command failed: %v\n", err)
			<-time.After(r.retryPeriod)
			res, err = r.client.IncrBy(key, int64(valueInt)).Result()
		}
		if err != nil {
			return err
		}

		part.SetBytes(strconv.AppendInt(nil, res, 10))
		return nil
	}
}

func getRedisOperator(opStr string) (redisOperator, error) {
	switch opStr {
	case "keys":
		return newRedisKeysOperator(), nil
	case "sadd":
		return newRedisSAddOperator(), nil
	case "scard":
		return newRedisSCardOperator(), nil
	case "incrby":
		return newRedisIncrByOperator(), nil
	}
	return nil, fmt.Errorf("operator not recognised: %v", opStr)
}

func (r *redisProc) execRaw(ctx context.Context, index int, inBatch service.MessageBatch, msg *service.Message) error {
	resMsg, err := inBatch.BloblangQuery(index, r.argsMapping)
	if err != nil {
		return fmt.Errorf("args mapping failed: %v", err)
	}

	iargs, err := resMsg.AsStructured()
	if err != nil {
		return err
	}

	args, ok := iargs.([]interface{})
	if !ok {
		return fmt.Errorf("mapping returned non-array result: %T", iargs)
	}
	for i, v := range args {
		n, isN := v.(json.Number)
		if !isN {
			continue
		}
		var nerr error
		if args[i], nerr = n.Int64(); nerr != nil {
			if args[i], nerr = n.Float64(); nerr != nil {
				args[i] = n.String()
			}
		}
	}

	command := inBatch.InterpolatedString(index, r.command)
	args = append([]interface{}{command}, args...)

	res, err := r.client.DoContext(ctx, args...).Result()
	for i := 0; i <= r.retries && err != nil; i++ {
		r.log.Errorf("%v command failed: %v", command, err)
		<-time.After(r.retryPeriod)
		res, err = r.client.DoContext(ctx, args...).Result()
	}
	if err != nil {
		return err
	}

	msg.SetStructured(res)
	return nil
}

func (r *redisProc) ProcessBatch(ctx context.Context, inBatch service.MessageBatch) ([]service.MessageBatch, error) {
	newMsg := inBatch.Copy()
	for index, part := range newMsg {
		if r.operator != nil {
			key := inBatch.InterpolatedString(index, r.key)
			if err := r.operator(r, key, part); err != nil {
				r.log.Debugf("Operator failed for key '%s': %v", key, err)
				part.SetError(fmt.Errorf("redis operator failed: %w", err))
			}
		} else {
			if err := r.execRaw(ctx, index, inBatch, part); err != nil {
				r.log.Debugf("Args mapping failed: %v", err)
				part.SetError(err)
			}
		}
	}
	return []service.MessageBatch{newMsg}, nil
}

func (r *redisProc) Close(ctx context.Context) error {
	return r.client.Close()
}
