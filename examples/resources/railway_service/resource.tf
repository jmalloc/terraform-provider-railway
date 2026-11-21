resource "railway_service" "example" {
  name       = "api"
  project_id = railway_project.example.id

  cpu_limit        = 1
  memory_limit_gb  = 0.5
  draining_seconds = 10
  overlap_seconds  = 0
}
