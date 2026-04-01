defmodule Arc.Host.Token do
  @moduledoc false

  @delegated_scopes [
    "resolve",
    "discover",
    "info",
    "send",
    "call",
    "stream",
    "tool.list",
    "tool.invoke"
  ]

  @admin_scope "admin"

  def admin_scope, do: @admin_scope
  def delegated_scopes, do: @delegated_scopes

  def generate_admin(opts \\ []) do
    generate(:admin, Keyword.put_new(opts, :scopes, [@admin_scope]))
  end

  def generate_delegated(opts \\ []) do
    scopes =
      opts
      |> Keyword.get(:scopes, @delegated_scopes)
      |> normalize_scopes()

    generate(:delegated, Keyword.put(opts, :scopes, scopes))
  end

  def hash(token) when is_binary(token) do
    :crypto.hash(:sha256, token)
  end

  def expired?(%{expires_at: nil}), do: false

  def expired?(%{expires_at: %DateTime{} = expires_at}) do
    DateTime.compare(expires_at, DateTime.utc_now()) == :lt
  end

  def allow_scope?(%{kind: :admin}, _scope), do: true

  def allow_scope?(%{scopes: scopes}, scope) when is_map(scopes) do
    MapSet.member?(scopes, scope)
  end

  def normalize_scopes(scopes) when is_list(scopes) do
    scopes
    |> Enum.map(&normalize_scope/1)
    |> Enum.reject(&is_nil/1)
    |> Enum.uniq()
    |> expand_all()
  end

  def normalize_scopes(_scopes), do: @delegated_scopes

  def document(%{identity: %Arc.Identity{} = identity} = record, raw_token) do
    %{
      "token" => raw_token,
      "id" => record.id,
      "kind" => Atom.to_string(record.kind),
      "label" => record.label,
      "identity" => identity_document(identity),
      "scopes" => scopes_list(record),
      "created_at" => DateTime.to_iso8601(record.created_at),
      "expires_at" => if(record.expires_at, do: DateTime.to_iso8601(record.expires_at), else: nil)
    }
  end

  def document(record, raw_token) do
    %{
      "token" => raw_token,
      "id" => record.id,
      "kind" => Atom.to_string(record.kind),
      "label" => record.label,
      "scopes" => scopes_list(record),
      "created_at" => DateTime.to_iso8601(record.created_at),
      "expires_at" => if(record.expires_at, do: DateTime.to_iso8601(record.expires_at), else: nil)
    }
  end

  def scopes_list(%{kind: :admin}), do: [@admin_scope]

  def scopes_list(%{scopes: scopes}) when is_map(scopes),
    do: scopes |> MapSet.to_list() |> Enum.sort()

  defp generate(kind, opts) do
    token =
      32
      |> :crypto.strong_rand_bytes()
      |> Base.url_encode64(padding: false)

    now = DateTime.utc_now()

    record = %{
      id:
        8
        |> :crypto.strong_rand_bytes()
        |> Base.encode16(case: :lower),
      kind: kind,
      hash: hash(token),
      identity: Keyword.get(opts, :identity),
      label: Keyword.get(opts, :label),
      scopes: MapSet.new(Keyword.get(opts, :scopes, [])),
      created_at: now,
      expires_at: expires_at(now, Keyword.get(opts, :ttl_seconds))
    }

    {token, record}
  end

  defp expires_at(_now, nil), do: nil

  defp expires_at(now, ttl_seconds) when is_integer(ttl_seconds) and ttl_seconds > 0 do
    DateTime.add(now, ttl_seconds, :second)
  end

  defp expires_at(_now, _ttl_seconds), do: nil

  defp normalize_scope(scope) when is_atom(scope), do: normalize_scope(Atom.to_string(scope))

  defp normalize_scope(scope) when is_binary(scope) do
    scope
    |> String.trim()
    |> String.downcase()
    |> case do
      "" -> nil
      "all" -> "*"
      other -> other
    end
  end

  defp normalize_scope(_scope), do: nil

  defp expand_all(scopes) do
    if "*" in scopes do
      @delegated_scopes
    else
      Enum.filter(scopes, &(&1 in @delegated_scopes))
    end
  end

  defp identity_document(identity) do
    %{
      "name" => Arc.Identity.name(identity),
      "short_name" => Arc.Identity.short_name(identity),
      "public_key" => Arc.Identity.encode_public_key(identity)
    }
  end
end
