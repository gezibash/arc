defmodule Arc.Data.HandlerTest do
  use ExUnit.Case, async: true

  alias Arc.Data.Handler

  test "resolve only accepts exec runtimes" do
    assert {:ok, Arc.Data.Handler.Exec, "exec:///tmp/runtime"} =
             Handler.resolve("exec:///tmp/runtime")

    assert {:error, {:unknown_scheme, "http"}} = Handler.resolve("http://localhost:4000")
    assert {:error, {:unknown_scheme, "sql"}} = Handler.resolve("sql:///tmp/demo.db")
  end
end
