defmodule Arc.Data.ToolboxTest do
  use ExUnit.Case, async: true

  alias Arc.Data.Toolbox

  defp tool(input, args) do
    %{
      "command" => "journal",
      "capability" => %{
        "invocation" => %{"method" => "RAW", "path" => "/"},
        "interfaces" => %{
          "cli" => %{
            "version" => 1,
            "namespace" => "journal",
            "commands" => [%{"path" => ["write"], "args" => args, "input" => input}]
          }
        }
      }
    }
  end

  @args [
    %{"name" => "addr", "kind" => "positional", "type" => "string", "required" => true},
    %{"name" => "body", "kind" => "option", "flag" => "--body", "type" => "string"},
    %{"name" => "draft", "kind" => "option", "flag" => "--draft", "type" => "boolean"}
  ]

  test "plain placeholders substitute raw values" do
    tool =
      tool(%{"source" => "template", "template" => "write {{addr}} --body \"{{body}}\""}, @args)

    assert {:ok, %{input: ~s(write inbox --body "He said "hi"")}} =
             Toolbox.build_invocation(tool, ["write", "inbox", "--body", ~s(He said "hi")])
  end

  test "json filter renders the value as a JSON literal" do
    tool =
      tool(%{"source" => "template", "template" => "write {{addr}} --body {{body|json}}"}, @args)

    assert {:ok, %{input: ~s(write inbox --body "He said \\"hi\\"\\n--flag")}} =
             Toolbox.build_invocation(tool, ["write", "inbox", "--body", "He said \"hi\"\n--flag"])
  end

  test "json filter renders a missing value as null" do
    tool = tool(%{"source" => "template", "template" => "write {{addr}} {{body|json}}"}, @args)

    assert {:ok, %{input: "write inbox null"}} =
             Toolbox.build_invocation(tool, ["write", "inbox"])
  end

  test "shell filter single-quotes the value" do
    tool =
      tool(%{"source" => "template", "template" => "write {{addr}} --body {{body|shell}}"}, @args)

    assert {:ok, %{input: ~s(write inbox --body 'it'\\''s "fine" --flag')}} =
             Toolbox.build_invocation(tool, ["write", "inbox", "--body", ~s(it's "fine" --flag)])
  end

  test "unknown filter is rejected" do
    tool = tool(%{"source" => "template", "template" => "write {{addr|base64}}"}, @args)

    assert {:error, {:invalid_template, "unknown filter base64"}} =
             Toolbox.build_invocation(tool, ["write", "inbox"])
  end

  test "json source sends every parsed argument as one JSON object" do
    tool = tool(%{"source" => "json"}, @args)

    assert {:ok, %{input: input}} =
             Toolbox.build_invocation(tool, [
               "write",
               "inbox",
               "--body",
               ~s(He said "hi"),
               "--draft"
             ])

    assert :json.decode(input) == %{
             "addr" => "inbox",
             "body" => ~s(He said "hi"),
             "draft" => true
           }
  end
end
