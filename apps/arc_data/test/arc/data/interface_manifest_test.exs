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

  test "normalizes a stdin input source with an optional header template" do
    cli =
      InterfaceManifest.cli(%{
        "interfaces" => %{
          "cli" => %{
            "namespace" => "pages",
            "commands" => [
              %{
                "path" => ["write"],
                "args" => [%{"name" => "path", "kind" => "positional", "required" => true}],
                "input" => %{"source" => "stdin", "template" => "POST /pages/{{path}}"}
              },
              %{"path" => ["raw"], "input" => %{"source" => "stdin"}}
            ]
          }
        }
      })

    [write, raw] = cli["commands"]

    assert write["input"] == %{
             "source" => "stdin",
             "template" => "POST /pages/{{path}}",
             "join_with" => "\n"
           }

    assert raw["input"] == %{"source" => "stdin", "join_with" => "\n"}
  end

  test "normalizes an events invoke mode with topic globs" do
    cli =
      InterfaceManifest.cli(%{
        "interfaces" => %{
          "cli" => %{
            "namespace" => "dm",
            "commands" => [
              %{"path" => ["watch"], "invoke" => %{"mode" => "events", "topics" => ["dm.*", ""]}},
              %{"path" => ["one"], "invoke" => %{"mode" => "events", "topics" => "x"}}
            ]
          }
        }
      })

    [watch, one] = cli["commands"]
    assert watch["invoke"] == %{"mode" => "events", "topics" => ["dm.*"]}
    assert one["invoke"] == %{"mode" => "events", "topics" => ["x"]}
  end

  test "keeps an explicit interface version and reports the max it renders" do
    cli =
      InterfaceManifest.cli(%{
        "interfaces" => %{
          "cli" => %{"namespace" => "dm", "version" => 2, "commands" => [%{"path" => ["x"]}]}
        }
      })

    assert cli["version"] == 2
    assert InterfaceManifest.max_cli_version() == 4
  end

  test "round trips version 3 private-file commands without changing legacy output filters" do
    cli =
      InterfaceManifest.cli(%{
        "interfaces" => %{
          "cli" => %{
            "version" => 3,
            "namespace" => "files",
            "commands" => [
              %{
                "path" => ["put"],
                "input" => %{"source" => "sealed_file", "file" => "path"},
                "output" => %{"private_file" => %{"operation" => "put"}}
              },
              %{
                "path" => ["get"],
                "input" => %{
                  "source" => "private_file",
                  "operation" => "get",
                  "id" => "id"
                },
                "output" => %{
                  "private_file" => %{"operation" => "get", "id" => "id", "path" => "output"}
                }
              },
              %{"path" => ["legacy"], "output" => %{"filter" => ["open", "preview:40"]}}
            ]
          }
        }
      })

    [put, get, legacy] = cli["commands"]

    assert cli["version"] == 3
    assert put["input"] == %{"source" => "sealed_file", "file" => "path"}

    assert put["output"] == %{
             "private_file" => %{"operation" => "put", "id" => nil, "path" => nil}
           }

    assert get["input"] == %{
             "source" => "private_file",
             "operation" => "get",
             "id" => "id",
             "after" => nil
           }

    assert get["output"] == %{
             "private_file" => %{"operation" => "get", "id" => "id", "path" => "output"}
           }

    assert legacy["output"] == %{"filters" => ["open", "preview:40"]}
    assert InterfaceManifest.cli(%{"interfaces" => %{"cli" => cli}}) == cli
  end

  test "the last positional stays variadic when options follow it" do
    cli =
      InterfaceManifest.cli(%{
        "interfaces" => %{
          "cli" => %{
            "namespace" => "dm",
            "commands" => [
              %{
                "path" => ["send"],
                "args" => [
                  %{"name" => "peer", "kind" => "positional", "required" => true},
                  %{"name" => "text", "kind" => "positional", "variadic" => true},
                  %{"name" => "file", "kind" => "option", "flag" => "--file"}
                ]
              }
            ]
          }
        }
      })

    [send] = cli["commands"]
    [peer, text, file] = send["args"]
    assert peer["variadic"] == false
    assert text["variadic"] == true
    assert file["kind"] == "option"
  end

  test "normalizes seal_to and an open output filter" do
    cli =
      InterfaceManifest.cli(%{
        "interfaces" => %{
          "cli" => %{
            "namespace" => "dm",
            "commands" => [
              %{
                "path" => ["send"],
                "args" => [%{"name" => "peer", "kind" => "positional", "required" => true}],
                "input" => %{
                  "source" => "stdin",
                  "template" => "send {{peer|pubkey}}",
                  "seal_to" => "peer"
                }
              },
              %{
                "path" => ["send2"],
                "input" => %{
                  "source" => "stdin",
                  "seal_to" => ["peer", "me"],
                  "body" => "text",
                  "file" => "file",
                  "attach" => "attach"
                }
              },
              %{"path" => ["read"], "output" => %{"filter" => "open"}},
              %{"path" => ["ls"], "output" => %{"filter" => "bogus"}},
              %{
                "path" => ["all"],
                "output" => %{"filter" => ["open", "petnames", "preview:80", "nope"]}
              }
            ]
          }
        }
      })

    [send, send2, read, ls, all] = cli["commands"]

    assert send["input"]["seal_to"] == ["peer"]
    assert send2["input"]["seal_to"] == ["peer", "me"]
    assert send2["input"]["body"] == "text"
    assert send2["input"]["file"] == "file"
    assert send2["input"]["attach"] == "attach"
    assert read["output"] == %{"filters" => ["open"]}
    refute Map.has_key?(ls, "output")
    assert all["output"] == %{"filters" => ["open", "petnames", "preview:80"]}

    # Normalizing an already normalized command is a no-op.
    assert InterfaceManifest.cli(%{"interfaces" => %{"cli" => cli}})["commands"] ==
             cli["commands"]
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

  test "journal write reads the body from stdin and renders a json header" do
    path = Path.expand("../../../../../providers/journal/manifest.json", __DIR__)

    assert {:ok, %{"cli" => cli}} = InterfaceManifest.load_file(path)
    assert write = Enum.find(cli["commands"], &(&1["path"] == ["write"]))
    assert %{"source" => "stdin", "join_with" => "\n", "template" => template} = write["input"]
    refute Enum.any?(write["args"], &(&1["name"] == "body"))

    values = %{"addr" => "hrs/ab/p1", "title" => ~s(P "one" --not-a-flag)}

    assert {:ok, header} = Arc.Data.Toolbox.render_template(template, values)

    assert header ==
             ~s(write hrs/ab/p1 --title "P \\"one\\" --not-a-flag" --tags null --if-rev null)
  end

  test "journal append and search render positionals as json" do
    path = Path.expand("../../../../../providers/journal/manifest.json", __DIR__)
    assert {:ok, %{"cli" => cli}} = InterfaceManifest.load_file(path)

    assert append = Enum.find(cli["commands"], &(&1["path"] == ["append"]))
    values = %{"addr" => "hrs/ab/p1", "text" => ["tried", "--lr", "3e-4,", "worse"]}
    assert {:ok, line} = Arc.Data.Toolbox.render_template(append["input"]["template"], values)
    assert line == ~s(append hrs/ab/p1 "tried --lr 3e-4, worse")

    assert search = Enum.find(cli["commands"], &(&1["path"] == ["search"]))
    values = %{"query" => "loss --deep dive", "project" => "hrs"}
    assert {:ok, line} = Arc.Data.Toolbox.render_template(search["input"]["template"], values)
    assert line == ~s(search "loss --deep dive" --project "hrs" --notebook "" --deep "")
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
