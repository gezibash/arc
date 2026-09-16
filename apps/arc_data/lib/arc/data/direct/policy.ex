defmodule Arc.Data.Direct.Policy do
  @moduledoc "Explicit owner permission for a peer, resource, and literal network addresses."

  @max_file_bytes 32_768
  @max_rules 32

  def load_file(path) when is_binary(path) do
    case File.open(path, [:read, :binary]) do
      {:ok, file} ->
        try do
          case IO.binread(file, @max_file_bytes + 1) do
            body when is_binary(body) and byte_size(body) <= @max_file_bytes ->
              decode(body)

            _ ->
              {:error, :invalid_direct_policy}
          end
        after
          File.close(file)
        end

      _ ->
        {:error, :direct_policy_read_failed}
    end
  end

  def load_file(_), do: {:error, :invalid_direct_policy}

  def decode(body) do
    case :json.decode(body) do
      %{"version" => 1, "rules" => rules} = document when map_size(document) == 2 ->
        normalize(rules)

      _ ->
        {:error, :invalid_direct_policy}
    end
  rescue
    _ -> {:error, :invalid_direct_policy}
  end

  def normalize(rules) when is_list(rules) and length(rules) <= @max_rules do
    result = Enum.map(rules, &rule/1)

    with true <- Enum.all?(result, &match?({:ok, _}, &1)),
         normalized <- Enum.map(result, &elem(&1, 1)),
         scopes <- Enum.map(normalized, &{&1.peer, &1.capability, &1.scheme, &1.path}),
         true <- length(Enum.uniq(scopes)) == length(scopes) do
      {:ok, normalized}
    else
      _ -> {:error, :invalid_direct_policy}
    end
  end

  def normalize(_), do: {:error, :invalid_direct_policy}

  def find(rules, peer, scope) do
    Enum.find(rules, fn rule ->
      rule.peer == peer and rule.capability == scope["capability"] and
        rule.scheme == scope["scheme"] and rule.path == scope["path"]
    end)
  end

  def candidate(rule, %{"host" => host, "port" => port} = candidate)
      when is_map(rule) and map_size(candidate) == 2 and is_integer(port) and port in 1..65_535 do
    with {:ok, ip} <- address(host),
         true <- ip in rule.dial do
      {:ok, ip, port}
    else
      _ -> {:error, :direct_address_denied}
    end
  end

  def candidate(_, _), do: {:error, :direct_address_denied}

  def format_ip(ip), do: ip |> :inet.ntoa() |> to_string()

  defp rule(
         %{
           peer: peer,
           capability: capability,
           scheme: scheme,
           path: path,
           lease_ms: lease,
           dial: dial,
           listen: listen
         } = normalized
       )
       when map_size(normalized) == 7 do
    # Revalidate programmatic callers through the same rules as file input.
    rule(%{
      "peer" => Base.encode16(peer, case: :lower),
      "capability" => capability,
      "scheme" => scheme,
      "path" => path,
      "lease_ms" => lease,
      "dial" => Enum.map(dial, &format_ip/1),
      "listen" =>
        if(listen,
          do: %{
            "bind" => format_ip(listen.ip),
            "address" => format_ip(listen.host),
            "port" => listen.port
          },
          else: nil
        )
    })
  rescue
    _ -> {:error, :invalid_direct_policy}
  end

  defp rule(
         %{"peer" => peer, "capability" => capability, "scheme" => scheme, "path" => path} = input
       ) do
    lease = Map.get(input, "lease_ms", 30_000)
    dial = Map.get(input, "dial", [])

    with true <-
           Enum.all?(
             Map.keys(input),
             &(&1 in ~w(peer capability scheme path lease_ms dial listen))
           ),
         true <- is_binary(peer) and byte_size(peer) == 64,
         {:ok, peer} <- Base.decode16(peer, case: :mixed),
         true <- is_binary(capability) and Regex.match?(~r/\A[a-zA-Z0-9_-]{1,64}\z/, capability),
         true <- is_integer(lease) and lease in 1_000..120_000,
         true <- is_binary(scheme) and is_binary(path) and String.starts_with?(path, "/"),
         {:ok, _} <- Arc.Data.Protocol.parse("#{scheme}+arc://#{Base.encode16(peer)}#{path}"),
         true <- is_list(dial) and length(dial) <= 4,
         {:ok, dial} <- addresses(dial),
         {:ok, listen} <- listener(input["listen"]),
         true <- dial != [] or not is_nil(listen) do
      {:ok,
       %{
         peer: peer,
         capability: capability,
         scheme: scheme,
         path: path,
         lease_ms: lease,
         dial: dial,
         listen: listen
       }}
    else
      _ -> {:error, :invalid_direct_policy}
    end
  end

  defp rule(_), do: {:error, :invalid_direct_policy}

  defp addresses(values) do
    parsed = Enum.map(values, &address/1)

    if Enum.all?(parsed, &match?({:ok, _}, &1)),
      do: {:ok, Enum.uniq(Enum.map(parsed, &elem(&1, 1)))},
      else: {:error, :invalid_direct_address}
  end

  defp listener(nil), do: {:ok, nil}

  defp listener(%{"bind" => bind, "address" => host, "port" => port} = value)
       when map_size(value) == 3 and is_integer(port) and port in 0..65_535 do
    with {:ok, ip} <- literal_ip(bind),
         {:ok, host} <- address(host),
         true <- tuple_size(ip) == tuple_size(host),
         true <- allowed_ip?(ip) or ip in [{0, 0, 0, 0}, {0, 0, 0, 0, 0, 0, 0, 0}] do
      {:ok, %{ip: ip, host: host, port: port}}
    else
      _ -> {:error, :invalid_direct_address}
    end
  end

  defp listener(_), do: {:error, :invalid_direct_address}

  defp address(value) do
    with {:ok, ip} <- literal_ip(value),
         true <- allowed_ip?(ip) do
      {:ok, ip}
    else
      _ -> {:error, :invalid_direct_address}
    end
  end

  defp literal_ip(value) when is_binary(value) and byte_size(value) <= 45,
    do: :inet.parse_strict_address(String.to_charlist(value))

  defp literal_ip(_), do: {:error, :invalid_direct_address}

  defp allowed_ip?({a, _, _, _}) when a == 0 or a >= 224, do: false
  defp allowed_ip?({169, 254, _, _}), do: false
  defp allowed_ip?({_, _, _, _}), do: true
  defp allowed_ip?({0, 0, 0, 0, 0, 0, 0, 0}), do: false
  defp allowed_ip?({0, 0, 0, 0, 0, 65_535, _, _}), do: false
  defp allowed_ip?({first, _, _, _, _, _, _, _}) when first >= 65_280, do: false
  defp allowed_ip?({first, _, _, _, _, _, _, _}) when first in 65_152..65_215, do: false
  defp allowed_ip?({_, _, _, _, _, _, _, _}), do: true
end
