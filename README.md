# PayFlow: End-to-End Payments Data Platform

A portfolio project that simulates a payment service provider (PSP) and builds the full data platform around it:
OLTP in PostgreSQL, CDC into Kafka, real-time fraud detection with Flink, a lakehouse and warehouse modelled with dbt,
batch reconciliation with Apache Beam, orchestration with Airflow on Kubernetes, infrastructure as code with Terraform,
CI/CD with GitHub Actions, and monitoring with Prometheus and Grafana.

> Status: **Phase 1 - OLTP + payment simulator** (see [docs/PROGRESS.md](docs/PROGRESS.md))

## Architecture (target)

```mermaid
flowchart LR
  SIM[Go payment simulator] --> PG[(PostgreSQL OLTP)]
  PG -- Debezium CDC --> K[[Kafka]]
  K --> FL[Flink: fraud + metrics]
  FL --> K
  K --> S3[(MinIO / S3 raw zone)]
  K --> KIN[[Kinesis via LocalStack]]
  S3 --> SF[(Snowflake / DuckDB)]
  SF --> DBT[dbt models + tests]
  S3 --> BEAM[Beam settlement job] --> BQ[(BigQuery)]
  AF{{Airflow on Kubernetes}} -.orchestrates.-> DBT & BEAM
  PROM[Prometheus + Grafana] -.monitors.-> SIM & K & FL & AF
```

## Quick start (Phase 1)

```bash
cp .env.example .env        # then edit the passwords
make sim-tidy               # download Go dependencies (first time)
make up                     # start postgres + simulator
make ps                     # both services should be "healthy"/"running"
make logs                   # watch the simulator's JSON logs
make validate               # run the data checks
make sim-metrics            # live counters from the simulator
```

## Repository layout

| Path | What lives there |
|---|---|
| `services/simulator/` | Go service that generates payment traffic |
| `infra/postgres/init/` | Schema and roles, applied on first database start |
| `infra/postgres/queries/` | Validation and analysis SQL |
| `docs/PROGRESS.md` | What is done and what is next |
| `docs/adr/` | Architecture Decision Records: why each choice was made |

## Make targets

Run `make help` for the full list.
