module github.com/vmkteam/ringsrv

go 1.26.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/ClickHouse/clickhouse-go/v2 v2.48.0
	github.com/getsentry/sentry-go v0.41.0
	github.com/go-pg/pg/v10 v10.15.0
	github.com/itchyny/gojq v0.12.19
	github.com/labstack/echo/v4 v4.15.0
	github.com/namsral/flag v1.7.4-pre
	github.com/prometheus/client_golang v1.23.2
	github.com/prometheus/client_model v0.6.2
	github.com/stretchr/testify v1.11.1
	github.com/vmkteam/appkit v0.1.2
	github.com/vmkteam/cron v0.1.5
	github.com/vmkteam/embedlog v0.1.3
	github.com/vmkteam/mcpkit v0.1.4
	github.com/vmkteam/zenrpc-middleware v1.3.2
	github.com/vmkteam/zenrpc/v2 v2.3.3
	golang.org/x/sync v0.22.0
)

require (
	github.com/ClickHouse/ch-go v0.74.0 // indirect
	github.com/adrg/frontmatter v0.2.0 // indirect
	github.com/andybalholm/brotli v1.2.2 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/coreos/go-oidc/v3 v3.17.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/getsentry/sentry-go/echo v0.41.0 // indirect
	github.com/go-faster/city v1.0.1 // indirect
	github.com/go-faster/errors v0.7.1 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-pg/zerochecker v0.2.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/itchyny/timefmt-go v0.1.8 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/labstack/gommon v0.4.2 // indirect
	github.com/lmittmann/tint v1.1.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/paulmach/orb v0.13.0 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/pierrec/lz4/v4 v4.1.27 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/common v0.67.5 // indirect
	github.com/prometheus/procfs v0.19.2 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fastjson v1.6.10 // indirect
	github.com/valyala/fasttemplate v1.2.2 // indirect
	github.com/vmihailenco/bufpool v0.1.11 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser v0.1.2 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	golang.org/x/tools v0.47.0 // indirect
	golang.org/x/xerrors v0.0.0-20200804184101-5ec99f83aff1 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v2 v2.3.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	mellium.im/sasl v0.3.2 // indirect
)

tool github.com/vmkteam/zenrpc/v2/zenrpc

replace github.com/go-pg/pg/v10 => github.com/vmkteam/pg/v10 v10.15.0-custom
