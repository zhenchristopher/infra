data "google_artifact_registry_docker_image" "api_image" {
  location      = var.gcp_region
  repository_id = var.core_repository_name
  image_name    = var.canary_release == null ? "api:latest" : "api@${var.canary_release.images.api.digest}"
}

data "google_artifact_registry_docker_image" "db_migrator_image" {
  location      = var.gcp_region
  image_name    = var.canary_release == null ? "db-migrator:latest" : "db-migrator@${var.canary_release.images["db-migrator"].digest}"
  repository_id = var.core_repository_name
}

data "google_artifact_registry_docker_image" "docker_reverse_proxy_image" {
  count         = var.docker_reverse_proxy_enabled ? 1 : 0
  location      = var.gcp_region
  image_name    = "docker-reverse-proxy:latest"
  repository_id = var.core_repository_name
}

data "google_artifact_registry_docker_image" "client_proxy_image" {
  location      = var.gcp_region
  image_name    = var.canary_release == null ? "client-proxy:latest" : "client-proxy@${var.canary_release.images["client-proxy"].digest}"
  repository_id = var.core_repository_name
}

data "google_artifact_registry_docker_image" "clickhouse_migrator_image" {
  location      = var.gcp_region
  image_name    = var.canary_release == null ? "clickhouse-migrator:latest" : "clickhouse-migrator@${var.canary_release.images["clickhouse-migrator"].digest}"
  repository_id = var.core_repository_name
}

data "google_artifact_registry_docker_image" "dashboard_api_image" {
  count         = var.dashboard_api_count > 0 ? 1 : 0
  location      = var.gcp_region
  repository_id = var.core_repository_name
  image_name    = var.canary_release == null ? "dashboard-api:latest" : "dashboard-api@${var.canary_release.images["dashboard-api"].digest}"
}
