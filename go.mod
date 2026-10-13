module lightmem

go 1.25

require github.com/lightmem/kernel v0.0.0

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20231201235250-de7065d80cb9 // indirect
	github.com/jackc/pgx/v5 v5.6.0 // indirect
	github.com/jackc/puddle/v2 v2.2.1 // indirect
	golang.org/x/crypto v0.23.0 // indirect
	golang.org/x/sync v0.7.0 // indirect
	golang.org/x/text v0.15.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/lightmem/kernel => ./kernel
