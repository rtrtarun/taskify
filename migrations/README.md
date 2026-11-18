# Migrations

SQL migrations applied with [golang-migrate](https://github.com/golang-migrate/migrate).

- Files are named `NNNNNN_name.up.sql` and `NNNNNN_name.down.sql`.
- Every migration has a working down script.
- Apply with `make migrate-up`; roll back one step with `make migrate-down`.

The first migration, `000001_init_tasks`, is added in milestone M1.
