module github.com/flowbyte-com/mpm/release_acceptance

go 1.26.1

require (
	github.com/flowbyte-com/mpm-core v0.0.0-00010101000000-000000000000
	github.com/mattn/go-sqlite3 v1.14.37
)

require (
	github.com/dlclark/regexp2 v1.11.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728 // indirect
	github.com/pkoukk/tiktoken-go v0.1.8 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/net v0.52.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/flowbyte-com/mpm-core => ../internal/core
