defmodule Arc.Data.InterfaceManifestTest do
  use ExUnit.Case, async: true

  alias Arc.Data.InterfaceManifest

  test "normalizes a legacy cli block into a versioned namespace interface" do
    capability = %{
      "cli" => %{
        "command" => %{
          "name" => "sqlite",
          "summary" => "Execute SQLite statements"
        },
        "args" => [
          %{
            "name" => "sql",
            "kind" => "positional",
            "required" => true,
            "variadic" => true
          }
        ],
        "input" => %{"source" => "template", "template" => "{{sql}}"}
      }
    }

    cli = InterfaceManifest.cli(capability)

    assert cli["version"] == 1
    assert cli["namespace"] == "sqlite"
    assert cli["summary"] == "Execute SQLite statements"
    assert hd(cli["commands"])["path"] == []
    assert hd(hd(cli["commands"])["args"])["name"] == "sql"
  end

  test "keeps a json input source" do
    capability = %{
      "cli" => %{
        "version" => 1,
        "namespace" => "journal",
        "commands" => [%{"path" => ["write"], "input" => %{"source" => "json"}}]
      }
    }

    cli = InterfaceManifest.cli(capability)

    assert hd(cli["commands"])["input"] == %{"source" => "json"}
  end

  test "loads a JSON interface manifest file" do
    path =
      Path.join(
        System.tmp_dir!(),
        "arc_interface_manifest_#{System.unique_integer([:positive])}.json"
      )

    File.write!(
      path,
      ~s({"cli":{"version":1,"namespace":"users","commands":[{"path":["get"],"input":{"source":"template","template":"GET /users"}}]}})
    )

    on_exit(fn -> File.rm(path) end)

    assert {:ok, %{"cli" => cli}} = InterfaceManifest.load_file(path)
    assert cli["namespace"] == "users"
    assert hd(cli["commands"])["path"] == ["get"]
  end

  test "loads a TOML interface manifest file" do
    path =
      Path.join(
        System.tmp_dir!(),
        "arc_interface_manifest_#{System.unique_integer([:positive])}.toml"
      )

    File.write!(
      path,
      """
      [cli]
      version = 1
      namespace = "users"

      [[cli.commands]]
      path = ["add"]
      summary = "Create a user"

      [cli.commands.input]
      source = "template"
      template = "POST /users"
      """
    )

    on_exit(fn -> File.rm(path) end)

    assert {:ok, %{"cli" => cli}} = InterfaceManifest.load_file(path)
    assert cli["namespace"] == "users"
    assert hd(cli["commands"])["path"] == ["add"]
  end
end
