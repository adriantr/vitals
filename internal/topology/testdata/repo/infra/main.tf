locals {
  services = {
    shop = {
      checkout = { cpu = 1 }
      web      = { cpu = 1 }
    }
  }
  access = {
    checkout = {
      spanner_databases = ["orders"]
      publishes         = ["orders-${var.env}"]
    }
  }
}
