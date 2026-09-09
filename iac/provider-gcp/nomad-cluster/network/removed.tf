removed {
  from = google_compute_security_policy.disable-bots-log-collector

  lifecycle {
    destroy = false
  }
}
