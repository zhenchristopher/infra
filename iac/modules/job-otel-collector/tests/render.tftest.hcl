# OpenTofu >= 1.12 or Terraform >= 1.7; mocked plans only, no infrastructure writes.
# tofu init -backend=false && tofu test -filter=tests/render.tftest.hcl
mock_provider "nomad" {}

variables {
  provider_name                  = "gcp"
  otel_collector_grpc_port        = 4317
  grafana_otlp_url                = " \t\n "
  grafana_username                = ""
  grafana_otel_collector_token    = ""
  scaffold_clickstack_otlp_endpoint = " https://clickstack.example.test/otel/ "
  scaffold_clickstack_otlp_token  = "test: token # \"quoted\""
  clickhouse_database            = "test"
  clickhouse_username            = "test"
  clickhouse_password            = "test"
}

run "clickstack_without_grafana_or_gcp_writes" {
  command = plan

  assert {
    condition = (
      toset(keys(yamldecode(local.otel_collector_config).exporters)) == toset(["clickhouse", "otlp_http/scaffold_clickstack"]) &&
      toset(keys(yamldecode(local.otel_collector_config).extensions)) == toset(["health_check"]) &&
      yamldecode(local.otel_collector_config).service.extensions == ["health_check"] &&
      yamldecode(local.otel_collector_config).exporters["otlp_http/scaffold_clickstack"].endpoint == "https://clickstack.example.test/otel" &&
      yamldecode(local.otel_collector_config).exporters["otlp_http/scaffold_clickstack"].headers.authorization == var.scaffold_clickstack_otlp_token
    )
    error_message = "Whitespace Grafana must disappear entirely; ClickStack must preserve its authenticated, normalized destination with no native GCP exporter."
  }

  assert {
    condition = alltrue([
      for name in ["metrics/scaffold", "metrics/scaffold_oom_kills", "metrics/prometheus", "metrics/rpc_only", "metrics/host", "traces", "logs"] :
      yamldecode(local.otel_collector_config).service.pipelines[name].exporters == ["otlp_http/scaffold_clickstack"]
    ])
    error_message = "All existing operational telemetry signals must reach ClickStack, not the product-only ClickHouse sink."
  }

  assert {
    condition = alltrue([
      for pipeline in values(yamldecode(local.otel_collector_config).service.pipelines) :
      length(pipeline.exporters) > 0 &&
      alltrue([for name in pipeline.exporters : contains(keys(yamldecode(local.otel_collector_config).exporters), name)]) &&
      alltrue([for name in pipeline.processors : contains(keys(yamldecode(local.otel_collector_config).processors), name)]) &&
      pipeline.processors[0] == "memory_limiter"
    ])
    error_message = "Every live pipeline must have real exporters, defined processors and admission limiting."
  }

  assert {
    condition = (
      contains(yamldecode(local.otel_collector_config).service.pipelines["metrics/scaffold"].processors, "filter/scaffold_non_oom_kills") &&
      contains(yamldecode(local.otel_collector_config).service.pipelines["metrics/scaffold_oom_kills"].processors, "filter/scaffold_oom_kills") &&
      contains(yamldecode(local.otel_collector_config).service.pipelines["metrics/scaffold_oom_kills"].processors, "cumulativetodelta/scaffold_oom_kills") &&
      yamldecode(local.otel_collector_config).processors["cumulativetodelta/scaffold_oom_kills"].initial_value == "keep"
    )
    error_message = "The dedicated OOM delta stream must remain separate from general startup metrics."
  }

  assert {
    condition = (
      yamldecode(local.otel_collector_config).processors.memory_limiter.limit_mib <= 384 &&
      yamldecode(local.otel_collector_config).exporters["otlp_http/scaffold_clickstack"].sending_queue.sizer == "bytes" &&
      yamldecode(local.otel_collector_config).exporters["otlp_http/scaffold_clickstack"].sending_queue.queue_size <= 8388608 &&
      yamldecode(local.otel_collector_config).exporters["otlp_http/scaffold_clickstack"].sending_queue.num_consumers <= 2 &&
      yamldecode(local.otel_collector_config).exporters["otlp_http/scaffold_clickstack"].sending_queue.batch.sizer == "bytes" &&
      yamldecode(local.otel_collector_config).exporters["otlp_http/scaffold_clickstack"].sending_queue.batch.max_size <= 1048576 &&
      yamldecode(local.otel_collector_config).exporters["otlp_http/scaffold_clickstack"].retry_on_failure.max_elapsed_time == "30s"
    )
    error_message = "Collector memory limiting, export byte queues, send concurrency and retries must remain bounded."
  }
}

run "grafana_compatibility" {
  command = plan
  variables {
    memory_mb                        = 1024
    grafana_otlp_url                  = " https://grafana.example.test/ "
    grafana_username                  = "tenant"
    grafana_otel_collector_token      = "grafana-token"
    scaffold_clickstack_otlp_endpoint = " "
  }
  assert {
    condition = (
      yamldecode(local.otel_collector_config).processors.memory_limiter.limit_mib == 768 &&
      yamldecode(local.otel_collector_config).processors.memory_limiter.spike_limit_mib == 192 &&
      toset(keys(yamldecode(local.otel_collector_config).exporters)) == toset(["clickhouse", "otlp_http/grafana_cloud"]) &&
      yamldecode(local.otel_collector_config).exporters["otlp_http/grafana_cloud"].endpoint == "https://grafana.example.test/otlp" &&
      yamldecode(local.otel_collector_config).extensions["basicauth/grafana_cloud"].client_auth.username == "tenant" &&
      yamldecode(local.otel_collector_config).extensions["basicauth/grafana_cloud"].client_auth.password == "grafana-token" &&
      yamldecode(local.otel_collector_config).exporters["otlp_http/grafana_cloud"].auth.authenticator == "basicauth/grafana_cloud" &&
      contains(yamldecode(local.otel_collector_config).service.extensions, "basicauth/grafana_cloud") &&
      alltrue([for name in ["metrics", "metrics/prometheus", "metrics/rpc_only", "metrics/host", "traces", "logs"] : yamldecode(local.otel_collector_config).service.pipelines[name].exporters == ["otlp_http/grafana_cloud"]])
    )
    error_message = "Valid Grafana-only deployments must retain authenticated export for all existing operational signals."
  }
}

run "both_destinations" {
  command = plan
  variables {
    grafana_otlp_url             = "https://grafana.example.test"
    grafana_username             = "tenant"
    grafana_otel_collector_token = "grafana-token"
  }
  assert {
    condition = (
      yamldecode(local.otel_collector_config).service.pipelines.metrics.exporters == ["otlp_http/grafana_cloud"] &&
      yamldecode(local.otel_collector_config).service.pipelines["metrics/scaffold"].exporters == ["otlp_http/scaffold_clickstack"] &&
      alltrue([for name in ["metrics/prometheus", "metrics/rpc_only", "metrics/host", "traces", "logs"] : toset(yamldecode(local.otel_collector_config).service.pipelines[name].exporters) == toset(["otlp_http/grafana_cloud", "otlp_http/scaffold_clickstack"])])
    )
    error_message = "Both explicitly configured destinations must receive operational telemetry without bypassing ClickStack's OOM delta separation."
  }
}

run "optional_sinks_disabled" {
  command = plan
  variables {
    scaffold_clickstack_otlp_endpoint = " \t "
  }
  assert {
    condition = (
      toset(keys(yamldecode(local.otel_collector_config).service.pipelines)) == toset(["metrics/external"]) &&
      yamldecode(local.otel_collector_config).service.pipelines["metrics/external"].exporters == ["clickhouse"] &&
      toset(keys(yamldecode(local.otel_collector_config).exporters)) == toset(["clickhouse"]) &&
      yamldecode(local.otel_collector_config).service.extensions == ["health_check"]
    )
    error_message = "No configured operational sink must omit its pipelines, not leave empty/dangling exporters or a silent debug fallback."
  }
}

run "explicit_gcp_and_router_opt_in" {
  command = plan
  variables {
    scaffold_clickstack_otlp_endpoint     = ""
    enable_gcp_telemetry_metrics          = true
    enable_gcp_telemetry_external_metrics = true
    gcp_telemetry_project_id              = "test-project"
    enable_otel_router_metrics           = true
  }
  assert {
    condition = (
      toset(keys(yamldecode(local.otel_collector_config).exporters)) == toset(["clickhouse", "googlemanagedprometheus/gcp_telemetry", "otlp_grpc/otel_router"]) &&
      alltrue([for name in ["metrics/gcp_telemetry", "metrics/gcp_telemetry/prometheus", "metrics/gcp_telemetry/rpc_only", "metrics/gcp_telemetry/host", "metrics/gcp_telemetry/external"] : yamldecode(local.otel_collector_config).service.pipelines[name].exporters == ["googlemanagedprometheus/gcp_telemetry"]]) &&
      yamldecode(local.otel_collector_config).service.pipelines["metrics/external/otel_router"].exporters == ["otlp_grpc/otel_router"] &&
      yamldecode(local.otel_collector_config).exporters["googlemanagedprometheus/gcp_telemetry"].project == "test-project"
    )
    error_message = "Existing native GCP and router metric opt-ins must remain independent of optional HTTP sinks."
  }
}

run "reject_invalid_clickstack_endpoint" {
  command = plan
  variables {
    scaffold_clickstack_otlp_endpoint = "https:// "
  }
  expect_failures = [nomad_job.otel_collector]
}

run "reject_missing_clickstack_auth" {
  command = plan
  variables {
    scaffold_clickstack_otlp_token = " \t "
  }
  expect_failures = [nomad_job.otel_collector]
}

run "reject_header_injection" {
  command = plan
  variables {
    scaffold_clickstack_otlp_token = "token\r\nInjected: value"
  }
  expect_failures = [nomad_job.otel_collector]
}

run "reject_non_http_grafana_endpoint" {
  command = plan
  variables {
    grafana_otlp_url             = "ftp://grafana.example.test"
    grafana_username             = "tenant"
    grafana_otel_collector_token = "token"
  }
  expect_failures = [nomad_job.otel_collector]
}

run "reject_missing_grafana_auth" {
  command = plan
  variables {
    grafana_otlp_url = "https://grafana.example.test"
  }
  expect_failures = [nomad_job.otel_collector]
}

run "aws_does_not_enable_gcp_export" {
  command = plan
  variables {
    provider_name               = "aws"
    enable_gcp_telemetry_metrics = true
    gcp_telemetry_project_id     = "unused"
  }
  assert {
    condition     = !contains(keys(yamldecode(local.otel_collector_config).exporters), "googlemanagedprometheus/gcp_telemetry")
    error_message = "The GCP metrics exporter must never render for AWS."
  }
}
