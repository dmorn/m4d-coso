env "local" {
  src = "file://db/schema.sql"
  url = "postgres://postgres:devpassword@localhost:5432/m4dtimes?sslmode=disable&search_path=public"
  dev = "postgres://postgres:devpassword@localhost:5432/atlas_dev?sslmode=disable&search_path=public"
  migration {
    dir = "file://db/migrations"
  }
}
