removed {
  from = google_secret_manager_secret.supabase_db_connection_string

  lifecycle {
    destroy = false
  }
}

removed {
  from = google_secret_manager_secret_version.supabase_db_connection_string

  lifecycle {
    destroy = false
  }
}

removed {
  from = google_secret_manager_secret.supabase_jwt_secrets

  lifecycle {
    destroy = false
  }
}

removed {
  from = google_secret_manager_secret_version.supabase_jwt_secrets

  lifecycle {
    destroy = false
  }
}
