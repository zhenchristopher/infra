variable "canary_release" {
  description = "Verified immutable release manifest; only the isolated canary may consume it."
  type = object({
    source       = object({ buildCommit = string })
    images       = map(object({ digest = string }))
    binaries     = map(object({ generation = string, sha256 = string }))
    machineImage = object({ name = string, id = string })
  })
  default = null

  validation {
    condition = var.canary_release == null ? true : (
      can(regex("^[a-f0-9]{40}$", var.canary_release.source.buildCommit)) &&
      can(regex("^[1-9][0-9]{0,29}$", var.canary_release.machineImage.id)) &&
      can(regex("^cny-orch-[0-9]{4}(-[0-9]{2}){5}$", var.canary_release.machineImage.name)) &&
      alltrue([for name in ["api", "db-migrator", "client-proxy", "clickhouse-migrator", "dashboard-api"] : can(regex("^sha256:[a-f0-9]{64}$", var.canary_release.images[name].digest))]) &&
      alltrue([for name in ["orchestrator", "template-manager", "envd", "clean-nfs-cache"] : can(regex("^[1-9][0-9]{0,29}$", var.canary_release.binaries[name].generation)) && can(regex("^[a-f0-9]{64}$", var.canary_release.binaries[name].sha256))])
    )
    error_message = "The canary release must contain exact source, image digests and binary generations."
  }
}

resource "terraform_data" "canary_release_boundary" {
  count = var.canary_release == null ? 0 : 1
  lifecycle {
    precondition {
      condition     = var.same_project_canary_enabled && var.gcp_project_id == "ashler-platform" && var.prefix == "cny-" && var.domain_name == "e2b-canary.ashler.com" && var.bucket_prefix == "ashler-platform-e2b-canary-"
      error_message = "Immutable canary releases cannot be applied to another environment."
    }
  }
}

data "google_compute_image" "canary_release" {
  count   = var.canary_release == null ? 0 : 1
  project = var.gcp_project_id
  name    = var.canary_release.machineImage.name
  lifecycle {
    postcondition {
      condition     = self.image_id == var.canary_release.machineImage.id && self.status == "READY"
      error_message = "Canary machine image numeric identity differs from the reviewed release."
    }
  }
}
