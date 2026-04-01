defmodule Arc.StorageTest do
  use ExUnit.Case
  doctest Arc.Storage

  test "greets the world" do
    assert Arc.Storage.hello() == :world
  end
end
