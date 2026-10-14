-- ==============================================================================
-- COSMOS WORKSTATION - SCYLLADB / CASSANDRA SCHEMA
-- Keyspace and table definitions for unified auth, S3 IAM, and telemetry
-- ==============================================================================

CREATE KEYSPACE IF NOT EXISTS cosmos 
WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1};

USE cosmos;

-- ─── USERS ───────────────────────────────────────────────────────────────────
-- Core user directory holding credentials, IAM keys, and bucket links
CREATE TABLE IF NOT EXISTS cosmos.users (
    service             text,
    email               text,
    name                text,
    password_hash       text,
    access_key          text,
    secret_key          text,
    bucket              text,
    bucket_display_name text,
    session_id          text,
    username            text,
    profile_image       text,
    created_at          bigint,
    PRIMARY KEY ((service, email))
);

CREATE INDEX IF NOT EXISTS users_session_idx ON cosmos.users (session_id);
CREATE INDEX IF NOT EXISTS users_username_idx ON cosmos.users (username);

-- ─── PENDING SIGNUPS ─────────────────────────────────────────────────────────
-- Holds temporary registration verification state & OTP tokens
CREATE TABLE IF NOT EXISTS cosmos.pending (
    service       text,
    reg_token     text,
    session_id    text,
    name          text,
    email         text,
    password_hash text,
    username      text,
    profile_image text,
    PRIMARY KEY ((service, reg_token))
);

-- ─── PASSWORD RESET TOKENS ───────────────────────────────────────────────────
-- One-time secure reset tokens with expiry
CREATE TABLE IF NOT EXISTS cosmos.password_reset_tokens (
    reset_token text PRIMARY KEY,
    service     text,
    email       text,
    expires_at  timestamp
);

-- ─── S3 BUCKETS REGISTRY ─────────────────────────────────────────────────────
-- Maps user-created buckets to owner accounts and friendly display names
CREATE TABLE IF NOT EXISTS cosmos.buckets (
    bucket_name  text PRIMARY KEY,
    owner_email  text,
    display_name text
);

CREATE INDEX IF NOT EXISTS buckets_owner_idx ON cosmos.buckets (owner_email);

-- ─── CUSTOM DOMAINS ──────────────────────────────────────────────────────────
-- Maps vanity / CNAME domains to specific private/public buckets
CREATE TABLE IF NOT EXISTS cosmos.bucket_domains (
    domain      text PRIMARY KEY,
    bucket_name text,
    owner_email text
);

CREATE INDEX IF NOT EXISTS bucket_domains_bucket_idx ON cosmos.bucket_domains (bucket_name);

-- ─── QUOTA REQUESTS ──────────────────────────────────────────────────────────
-- Storage capacity expansion requests pending administrator approval
CREATE TABLE IF NOT EXISTS cosmos.quota_requests (
    req_token text PRIMARY KEY,
    email     text,
    amount    int
);

-- ─── S3 ALL-TIME METRICS ─────────────────────────────────────────────────────
-- Aggregated operational telemetry per bucket
CREATE TABLE IF NOT EXISTS cosmos.s3_analytics (
    bucket_name    text PRIMARY KEY,
    total_requests bigint,
    cache_hits     bigint,
    origin_hits    bigint,
    total_success  bigint,
    total_4xx      bigint,
    total_5xx      bigint,
    total_bytes    bigint,
    last_hit       bigint
);

-- ─── S3 HOURLY TIME-SERIES METRICS ───────────────────────────────────────────
-- Granular hourly counters for latency, bandwidth, and traffic trends
CREATE TABLE IF NOT EXISTS cosmos.s3_analytics_hourly (
    bucket_name text,
    hour        timestamp,
    requests    counter,
    cache_hits  counter,
    origin_hits counter,
    reads       counter,
    writes      counter,
    errors      counter,
    bytes       counter,
    PRIMARY KEY (bucket_name, hour)
);
