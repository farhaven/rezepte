-- name: MigrationApplied :one
select exists (select 1 from migrations where name = ?1);

-- name: MarkMigrationApplied :exec
insert into migrations (name) values (?1);