variable "canary_release" {
  type = object({
    source   = object({ buildCommit = string })
    images   = map(object({ digest = string }))
    binaries = map(object({ generation = string, sha256 = string }))
  })
  default = null
}

data "google_storage_bucket_object" "canary_envd" {
  count  = var.canary_release == null ? 0 : 1
  name   = "releases/${var.canary_release.source.buildCommit}/envd"
  bucket = var.fc_env_pipeline_bucket_name
  lifecycle {
    postcondition {
      condition     = tostring(self.generation) == var.canary_release.binaries.envd.generation
      error_message = "Canary EnvD generation differs from the reviewed release."
    }
  }
}
