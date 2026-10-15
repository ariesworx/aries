module github.com/ariesworx/aries/spike/https

go 1.27

require (
	github.com/ariesworx/aries v0.0.0
	github.com/ariesworx/aries/spike/identity v0.0.0
	github.com/modelcontextprotocol/go-sdk v1.8.0
	golang.org/x/crypto v0.58.0
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace (
	github.com/ariesworx/aries => ../..
	github.com/ariesworx/aries/spike/identity => ../identity
)
