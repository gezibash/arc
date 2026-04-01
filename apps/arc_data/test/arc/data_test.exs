defmodule Arc.DataTest do
  use ExUnit.Case

  test "data plane module exists" do
    assert is_atom(Arc.Data)
  end
end
