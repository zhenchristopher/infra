moved {
  from = google_project_service.secrets_manager_api
  to   = google_project_service.secrets_manager_api[0]
}

moved {
  from = google_project_service.certificate_manager_api
  to   = google_project_service.certificate_manager_api[0]
}

moved {
  from = google_project_service.compute_engine_api
  to   = google_project_service.compute_engine_api[0]
}

moved {
  from = google_project_service.artifact_registry_api
  to   = google_project_service.artifact_registry_api[0]
}

moved {
  from = google_project_service.os_config_api
  to   = google_project_service.os_config_api[0]
}

moved {
  from = google_project_service.monitoring_api
  to   = google_project_service.monitoring_api[0]
}

moved {
  from = google_project_service.logging_api
  to   = google_project_service.logging_api[0]
}

moved {
  from = google_project_service.filestore_api
  to   = google_project_service.filestore_api[0]
}

moved {
  from = google_secret_manager_secret.cloudflare_api_token
  to   = google_secret_manager_secret.cloudflare_api_token[0]
}

moved {
  from = google_secret_manager_secret.postgres_connection_string
  to   = google_secret_manager_secret.postgres_connection_string[0]
}
