package pure

import (
	"context"
	"fmt"
	"time"

	"github.com/benthosdev/benthos/v4/internal/bloblang/field"
	"github.com/benthosdev/benthos/v4/internal/bundle"
	"github.com/benthosdev/benthos/v4/internal/component/cache"
	"github.com/benthosdev/benthos/v4/internal/component/output"
	"github.com/benthosdev/benthos/v4/internal/component/output/processors"
	"github.com/benthosdev/benthos/v4/internal/docs"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/message"
)

func init() {
	err := bundle.AllOutputs.Add(processors.WrapConstructor(func(c output.Config, nm bundle.NewManagement) (output.Streamed, error) {
		ca, err := NewCacheWriter(c.Cache, nm, nm.Logger())
		if err != nil {
			return nil, err
		}
		return output.NewAsyncWriter("cache", c.Cache.MaxInFlight, ca, nm)
	}), docs.ComponentSpec{
		Name:    "cache",
		Summary: `Stores each message in a [cache](/docs/components/caches/about).`,
		Description: output.Description(true, false, `Caches are configured as [resources](/docs/components/caches/about), where there's a wide variety to choose from.

The `+"`target`"+` field must reference a configured cache resource label like follows:

`+"```yaml"+`
output:
  cache:
    target: foo
    key: ${!json("document.id")}

cache_resources:
  - label: foo
    memcached:
      addresses:
        - localhost:11211
      default_ttl: 60s
`+"```"+`

In order to create a unique `+"`key`"+` value per item you should use function interpolations described [here](/docs/configuration/interpolation#bloblang-queries).`),
		Config: docs.FieldComponent().WithChildren(
			docs.FieldString("target", "The target cache to store messages in."),
			docs.FieldString("key", "The key to store messages by, function interpolation should be used in order to derive a unique key for each message.",
				`${!count("items")}-${!timestamp_unix_nano()}`,
				`${!json("doc.id")}`,
				`${!meta("kafka_key")}`,
			).IsInterpolated(),
			docs.FieldString(
				"ttl", "The TTL of each individual item as a duration string. After this period an item will be eligible for removal during the next compaction. Not all caches support per-key TTLs, and those that do not will fall back to their generally configured TTL setting.",
				"60s", "5m", "36h",
			).IsInterpolated().AtVersion("3.33.0").Advanced(),
			docs.FieldInt("max_in_flight", "The maximum number of messages to have in flight at a given time. Increase this to improve throughput."),
		).ChildDefaultAndTypesFromStruct(output.NewCacheConfig()),
		Categories: []string{
			"Services",
		},
	})
	if err != nil {
		panic(err)
	}
}

// CacheWriter implements an output writer for caches.
type CacheWriter struct {
	conf output.CacheConfig
	mgr  bundle.NewManagement

	key *field.Expression
	ttl *field.Expression

	log log.Modular
}

// NewCacheWriter creates a writer for cache the output plugin.
func NewCacheWriter(conf output.CacheConfig, mgr bundle.NewManagement, log log.Modular) (*CacheWriter, error) {
	key, err := mgr.BloblEnvironment().NewField(conf.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key expression: %v", err)
	}
	ttl, err := mgr.BloblEnvironment().NewField(conf.TTL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ttl expression: %v", err)
	}
	if !mgr.ProbeCache(conf.Target) {
		return nil, fmt.Errorf("cache resource '%v' was not found", conf.Target)
	}
	return &CacheWriter{
		conf: conf,
		mgr:  mgr,
		key:  key,
		ttl:  ttl,
		log:  log,
	}, nil
}

// ConnectWithContext does nothing.
func (c *CacheWriter) ConnectWithContext(ctx context.Context) error {
	c.log.Infof("Writing message parts as items in cache: %v\n", c.conf.Target)
	return nil
}

func (c *CacheWriter) writeMulti(ctx context.Context, msg *message.Batch) error {
	var err error
	if cerr := c.mgr.AccessCache(ctx, c.conf.Target, func(ac cache.V1) {
		items := map[string]cache.TTLItem{}
		if err = msg.Iter(func(i int, p *message.Part) error {
			var ttl *time.Duration
			if ttls := c.ttl.String(i, msg); ttls != "" {
				t, terr := time.ParseDuration(ttls)
				if terr != nil {
					c.log.Debugf("Invalid duration string for TTL field: %v\n", terr)
					return fmt.Errorf("ttl field: %w", terr)
				}
				ttl = &t
			}
			items[c.key.String(i, msg)] = cache.TTLItem{
				Value: p.Get(),
				TTL:   ttl,
			}
			return nil
		}); err != nil {
			return
		}
		err = ac.SetMulti(ctx, items)
	}); cerr != nil {
		err = cerr
	}
	return err
}

// WriteWithContext attempts to store a message within a cache.
func (c *CacheWriter) WriteWithContext(ctx context.Context, msg *message.Batch) error {
	if msg.Len() > 1 {
		return c.writeMulti(ctx, msg)
	}
	var err error
	if cerr := c.mgr.AccessCache(ctx, c.conf.Target, func(cache cache.V1) {
		var ttl *time.Duration
		if ttls := c.ttl.String(0, msg); ttls != "" {
			t, terr := time.ParseDuration(ttls)
			if terr != nil {
				c.log.Debugf("Invalid duration string for TTL field: %v\n", terr)
				err = fmt.Errorf("ttl field: %w", terr)
				return
			}
			ttl = &t
		}
		err = cache.Set(ctx, c.key.String(0, msg), msg.Get(0).Get(), ttl)
	}); cerr != nil {
		err = cerr
	}
	return err
}

// CloseAsync does nothing.
func (c *CacheWriter) CloseAsync() {
}

// WaitForClose does nothing.
func (c *CacheWriter) WaitForClose(time.Duration) error {
	return nil
}
