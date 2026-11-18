module magic

go 1.26.2

replace github.com/xtaci/kcp-go/v5 => ./third_party/kcp-go

require (
	github.com/ebitengine/purego v0.11.3
	github.com/xtaci/kcp-go/v5 v5.7.5
	golang.org/x/crypto v0.58.0
	golang.org/x/sys v0.50.0
)

require (
	github.com/juju/ratelimit v1.0.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.1 // indirect
	github.com/klauspost/reedsolomon v1.14.2 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/pion/transport/v5 v5.1.4 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/tjfoc/gmsm v1.4.1 // indirect
	golang.org/x/net v0.61.0 // indirect
)
