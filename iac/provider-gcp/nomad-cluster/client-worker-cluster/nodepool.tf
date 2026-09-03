locals {
  has_local_ssd = var.cache_disks.disk_type == "local-ssd"
  startup_script = templatefile("${path.module}/../scripts/start-client.sh", {
    CLUSTER_TAG_NAME                  = var.cluster_tag_name
    SCRIPTS_BUCKET                    = var.cluster_setup_bucket_name
    FC_KERNELS_BUCKET_NAME            = var.fc_kernels_bucket_name
    FC_VERSIONS_BUCKET_NAME           = var.fc_versions_bucket_name
    FC_ENV_PIPELINE_BUCKET_NAME       = var.fc_env_pipeline_bucket_name
    FC_BUSYBOX_BUCKET_NAME            = var.fc_busybox_bucket_name
    DOCKER_CONTEXTS_BUCKET_NAME       = var.docker_contexts_bucket_name
    GCP_REGION                        = var.gcp_region
    GOOGLE_SERVICE_ACCOUNT_KEY        = var.google_service_account_key
    NOMAD_TOKEN                       = var.nomad_acl_token_secret
    CONSUL_TOKEN                      = var.consul_acl_token_secret
    RUN_CONSUL_FILE_HASH              = var.file_hash["scripts/run-consul.sh"]
    RUN_NOMAD_FILE_HASH               = var.file_hash["scripts/run-nomad.sh"]
    CONSUL_GOSSIP_ENCRYPTION_KEY      = var.consul_gossip_encryption_key_secret_data
    CONSUL_DNS_REQUEST_TOKEN          = var.consul_dns_request_token_secret_data
    NFS_IP_ADDRESS                    = var.filestore_cache_enabled ? join(",", var.nfs_ip_addresses) : ""
    NFS_MOUNT_PATH                    = var.nfs_mount_path
    NFS_MOUNT_SUBDIR                  = var.nfs_mount_subdir
    NFS_MOUNT_OPTS                    = var.nfs_mount_opts
    USE_FILESTORE_CACHE               = var.filestore_cache_enabled
    NODE_POOL                         = var.node_pool
    BASE_HUGEPAGES_PERCENTAGE         = var.base_hugepages_percentage
    CACHE_DISK_COUNT                  = var.cache_disks.count
    LOCAL_SSD                         = local.has_local_ssd ? "true" : "false"
    SET_ORCHESTRATOR_VERSION_METADATA = var.set_orchestrator_version_metadata ? "true" : "false"
    NODE_LABELS                       = join(",", var.node_labels)
    PERSISTENT_VOLUME_TYPES           = var.persistent_volume_types
  })
}


resource "google_compute_health_check" "nomad_check" {
  name                = "${var.cluster_name}-nomad-client-check"
  check_interval_sec  = 15
  timeout_sec         = 10
  healthy_threshold   = 2
  unhealthy_threshold = 10 # 50 seconds

  log_config {
    enable = true
  }

  http_health_check {
    request_path = "/v1/agent/health"
    port         = var.nomad_port
  }
}

resource "google_compute_region_instance_group_manager" "pool" {
  name   = "${var.cluster_name}-rig"
  region = var.gcp_region

  target_size = var.cluster_size

  version {
    name              = google_compute_instance_template.template.id
    instance_template = google_compute_instance_template.template.id
  }

  auto_healing_policies {
    health_check      = google_compute_health_check.nomad_check.id
    initial_delay_sec = 600
  }

  distribution_policy_target_shape = "BALANCED"

  # Server is a stateful cluster, so the update strategy used to roll out a new GCE Instance Template must be
  # a rolling update.
  update_policy {
    type                         = var.private_nodes_enabled || var.environment != "dev" ? "OPPORTUNISTIC" : "PROACTIVE"
    minimal_action               = "REPLACE"
    max_surge_fixed              = 10
    max_surge_percent            = null
    max_unavailable_fixed        = 5
    max_unavailable_percent      = null
    replacement_method           = "SUBSTITUTE"
    instance_redistribution_type = "NONE"
  }

  base_instance_name = var.cluster_name
  target_pools       = []

  depends_on = [
    google_compute_instance_template.template,
  ]

  lifecycle {
    ignore_changes = [target_size]

    precondition {
      condition     = var.capacity_manager_max_size == 10
      error_message = "Client capacity manager max size must remain locked at 10."
    }

    precondition {
      condition     = var.capacity_manager_max_size >= var.cluster_size
      error_message = "Client capacity manager max size must be greater than or equal to cluster_size."
    }
  }
}

data "google_compute_image" "source_image" {
  name   = var.image_name != "" ? var.image_name : null
  family = var.image_name == "" ? var.image_family : null
}

resource "google_compute_instance_template" "template" {
  name_prefix = "${var.cluster_name}-"

  instance_description = null
  machine_type         = var.machine_type
  min_cpu_platform     = var.min_cpu_platform

  labels = merge(
    var.labels,
    (var.environment != "dev" ? {
      goog-ops-agent-policy = "v2-x86-template-1-2-0-${var.gcp_zone}"
    } : {})
  )
  tags                    = [var.cluster_tag_name]
  metadata_startup_script = local.startup_script
  metadata = {
    enable-osconfig         = "TRUE",
    enable-guest-attributes = "TRUE",
  }

  scheduling {
    on_host_maintenance = "MIGRATE"
  }

  disk {
    auto_delete  = true
    boot         = true
    source_image = data.google_compute_image.source_image.id
    disk_size_gb = var.boot_disk.size_gb
    disk_type    = var.boot_disk.disk_type
  }

  # Cache disks - Local SSDs
  dynamic "disk" {
    for_each = [
      for _ in range(local.has_local_ssd ? var.cache_disks.count : 0) : {}
    ]

    content {
      auto_delete  = true
      boot         = false
      disk_size_gb = var.cache_disks.size_gb
      interface    = "NVME"
      disk_type    = var.cache_disks.disk_type
      type         = "SCRATCH"
    }
  }

  # Cache Disk - Persistent Disk
  dynamic "disk" {
    for_each = [for n in range(!local.has_local_ssd ? 1 : 0) : {}]
    content {
      auto_delete  = true
      boot         = false
      type         = "PERSISTENT"
      disk_size_gb = var.cache_disks.size_gb
      disk_type    = var.cache_disks.disk_type
    }
  }

  network_interface {
    network  = var.network_name
    nic_type = var.network_interface_type

    dynamic "access_config" {
      for_each = var.private_nodes_enabled ? [] : ["public_ip"]
      content {}
    }
  }

  # For a full list of oAuth 2.0 Scopes, see https://developers.google.com/identity/protocols/googlescopes
  service_account {
    email = var.google_service_account_email
    scopes = [
      "userinfo-email",
      "compute-ro",
      "https://www.googleapis.com/auth/logging.write",
      "https://www.googleapis.com/auth/monitoring.write",
      "https://www.googleapis.com/auth/trace.append",
      "https://www.googleapis.com/auth/cloud-platform"
    ]
  }

  # Per Terraform Docs (https://www.terraform.io/docs/providers/google/r/compute_instance_template.html#using-with-instance-group-manager),
  # we need to create a new instance template before we can destroy the old one. Note that any Terraform resource on
  # which this Terraform resource depends will also need this lifecycle statement.
  lifecycle {
    precondition {
      condition     = local.has_local_ssd || var.cache_disks.count == 1
      error_message = "When using persistent disks for the cluster cache, only 1 disk is supported."
    }

    precondition {
      condition     = !local.has_local_ssd || var.cache_disks.size_gb == 375
      error_message = "When using local-ssd for the cluster cache, each disk must be exactly 375 GB."
    }

    create_before_destroy = true
  }
}
