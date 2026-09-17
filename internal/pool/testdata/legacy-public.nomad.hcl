job "legacy-api" {
  datacenters = ["pool"]
  type        = "service"

  group "web" {
    constraint {
      attribute = "$${node.unique.name}"
      operator  = "="
      value     = "oracle-main"
    }

    network {
      port "http" {
        to           = 8080
        host_network = "wireguard"
      }
    }

    service {
      name     = "legacy-api"
      provider = "nomad"
      port     = "http"

      tags = [
        "traefik.enable=true",
        "traefik.http.routers.legacy-api.rule=Host(`legacy.example.com`)",
        "traefik.http.routers.legacy-api.entrypoints=web",
        "traefik.http.routers.legacy-api-secure.rule=Host(`legacy.example.com`)",
        "traefik.http.routers.legacy-api-secure.entrypoints=websecure",
        "traefik.http.routers.legacy-api-secure.tls=true",
        "traefik.http.routers.legacy-api-secure.tls.certresolver=letsencrypt",
      ]

      check {
        type     = "http"
        path     = "/health"
        interval = "15s"
        timeout  = "3s"
      }
    }

    task "app" {
      driver = "docker"

      config {
        image = "ghcr.io/example/backend:one"
        ports = ["http"]

      }

      env {
        MODE = "production"
      }


      resources {
        cpu    = 500
        memory = 512
      }
    }
  }
}
