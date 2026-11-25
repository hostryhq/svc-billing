-- Cluster provisioning of the billing database roles (ADR-0003 section 6, ADR-0014 A1.4 item 2). Run once as a superuser per
-- cluster, before `billing migrate`; deployment owns it (AM12: not run anywhere yet).
--
--   psql -v migrator_password=... -v app_password=... -v global_password=... -v relay_password=... -f provision-roles.sql
--
-- Passwords come from the secret store by reference; never put a value in this file.
--
--   billing_migrator  owns the database and its tables; runs migrations only (never at serve time)
--   billing_app       DML on the tenant-owned tables, NOSUPERUSER NOBYPASSRLS, not an owner (the runtime role)
--   billing_global    the service-global tables only: rule master, platform vendor log, used grants, emissions
--   billing_relay     the outbox table only (relay to the bus)

SELECT format('CREATE ROLE billing_migrator LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'migrator_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_migrator') \gexec
SELECT format('CREATE ROLE billing_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'app_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_app') \gexec
SELECT format('CREATE ROLE billing_global LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'global_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_global') \gexec
SELECT format('CREATE ROLE billing_relay LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'relay_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'billing_relay') \gexec

SELECT 'CREATE DATABASE billing OWNER billing_migrator'
 WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'billing') \gexec
