
# Enable Secrets Manager API
resource "google_project_service" "secrets_manager_api" {
  count   = var.manage_project_services ? 1 : 0
  service = "secretmanager.googleapis.com"

  disable_on_destroy = false
}

# Enable Certificate Manager API
resource "google_project_service" "certificate_manager_api" {
  count = var.manage_project_services ? 1 : 0
  #project = var.gcp_project_id
  service = "certificatemanager.googleapis.com"

  disable_on_destroy = false
}

# Enable Compute Engine API
resource "google_project_service" "compute_engine_api" {
  count = var.manage_project_services ? 1 : 0
  #project = var.gcp_project_id
  service = "compute.googleapis.com"

  disable_on_destroy = false
}

# Enable Artifact Registry API
resource "google_project_service" "artifact_registry_api" {
  count = var.manage_project_services ? 1 : 0
  #project = var.gcp_project_id
  service = "artifactregistry.googleapis.com"

  disable_on_destroy = false
}

# Enable OS Config API
resource "google_project_service" "os_config_api" {
  count = var.manage_project_services ? 1 : 0
  #project = var.gcp_project_id
  service = "osconfig.googleapis.com"

  disable_on_destroy = false
}

# Enable Stackdriver Monitoring API
resource "google_project_service" "monitoring_api" {
  count = var.manage_project_services ? 1 : 0
  #project = var.gcp_project_id
  service = "monitoring.googleapis.com"

  disable_on_destroy = false
}

# Enable Stackdriver Logging API
resource "google_project_service" "logging_api" {
  count = var.manage_project_services ? 1 : 0
  #project = var.gcp_project_id
  service = "logging.googleapis.com"

  disable_on_destroy = false
}

# Enable Filestore API
resource "google_project_service" "filestore_api" {
  count = var.manage_project_services ? 1 : 0
  #project = var.gcp_project_id
  service = "file.googleapis.com"

  disable_on_destroy = false
}

resource "time_sleep" "secrets_api_wait_60_seconds" {
  depends_on = [google_project_service.secrets_manager_api]

  create_duration = "60s"
}

resource "google_service_account" "infra_instances_service_account" {
  account_id   = "${var.prefix}infra-instances"
  display_name = "Infra Instances Service Account"
}

resource "google_service_account_key" "google_service_key" {
  service_account_id = google_service_account.infra_instances_service_account.name
}

// todo: delete after migration period
resource "google_artifact_registry_repository" "orchestration_repository" {
  format        = "DOCKER"
  repository_id = var.orchestration_repository_id
  labels        = var.labels

  depends_on = [time_sleep.artifact_registry_api_wait_90_seconds]
}

resource "time_sleep" "artifact_registry_api_wait_90_seconds" {
  depends_on = [google_project_service.artifact_registry_api]

  create_duration = "90s"
}

resource "google_artifact_registry_repository_iam_member" "orchestration_repository_member" {
  repository = google_artifact_registry_repository.orchestration_repository.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.infra_instances_service_account.email}"

  depends_on = [time_sleep.artifact_registry_api_wait_90_seconds]
}

resource "google_artifact_registry_repository" "core" {
  format        = "DOCKER"
  repository_id = "${var.prefix}core"
  labels        = var.labels

  depends_on = [time_sleep.artifact_registry_api_wait_90_seconds]
}

resource "google_artifact_registry_repository_iam_member" "core" {
  repository = google_artifact_registry_repository.core.name
  role       = "roles/artifactregistry.reader"
  member     = "serviceAccount:${google_service_account.infra_instances_service_account.email}"

  depends_on = [time_sleep.artifact_registry_api_wait_90_seconds]
}
