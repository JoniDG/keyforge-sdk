module github.com/JoniDG/keyforge-sdk/examples/go/spotify

go 1.24

replace github.com/JoniDG/keyforge-sdk/go => ../../../go

require (
	github.com/JoniDG/keyforge-protocol/go v0.10.0
	github.com/JoniDG/keyforge-sdk/go v0.0.0-00010101000000-000000000000
	github.com/stretchr/testify v1.12.1
)

require (
	github.com/coder/websocket v1.8.14 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)
