resource "railway_project" "example" {
  name         = "example"
  workspace_id = var.railway_workspace_id

  default_environment = {
    name = "production"
  }
}

resource "railway_project_token" "ci" {
  name           = "CI"
  project_id     = railway_project.example.id
  environment_id = railway_project.example.default_environment.id
}

output "railway_token" {
  value     = railway_project_token.ci.token
  sensitive = true
}
