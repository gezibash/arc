defmodule Arc.Net.Direct do
  @moduledoc """
  Bounded, mutually authenticated TLS carrier for a single promoted ARC route.

  This module deliberately knows nothing about relay negotiation, grants, or
  route policy. Callers must provide the already-agreed peer key, certificate
  fingerprint, and context binding. A connection reports readiness only after
  the TLS peer certificate and the ARC identity proof both match those values.
  """

  alias Arc.Net.Direct.Connection
  alias Arc.Net.Direct.Listener
  alias Arc.Net.Direct.Punch

  @max_packet_bytes 1_114_112
  @default_timeout_ms 5_000

  @type credentials :: %{
          required(:cert_der) => binary(),
          required(:private_key) => term(),
          required(:fingerprint) => <<_::256>>
        }

  @doc "Generate a short-lived in-memory TLS credential with OTP public_key."
  @spec credentials() :: {:ok, credentials()} | {:error, term()}
  def credentials do
    # OTP generates a fresh RSA credential wholly in memory. Its certificate
    # is explicitly short lived and pinned by SHA-256 below, so its test CA
    # name is never used as an ARC identity or trust namespace.
    today = Date.utc_today()
    # pkix_test_data encodes date-only bounds at 13:00 UTC. Start yesterday
    # so a credential made before that hour is immediately usable; it expires
    # tomorrow at 13:00 UTC, keeping its lifetime under 48 hours.
    validity =
      {today |> Date.add(-1) |> Date.to_erl(), today |> Date.add(1) |> Date.to_erl()}

    %{server_config: config} =
      :public_key.pkix_test_data(%{
        # TLS 1.3 rejects the tiny historical EC key used by the public_key
        # defaults. Give this ephemeral certificate a TLS 1.3-compatible key.
        server_chain: %{
          root: [key: {:rsa, 2_048, 65_537}, digest: :sha256, validity: validity],
          intermediates: [],
          peer: [key: {:rsa, 2_048, 65_537}, digest: :sha256, validity: validity]
        },
        client_chain: %{root: [], intermediates: [], peer: []}
      })

    cert = Keyword.fetch!(config, :cert)
    key = Keyword.fetch!(config, :key)
    {:ok, %{cert_der: cert, private_key: key, fingerprint: :crypto.hash(:sha256, cert)}}
  rescue
    error -> {:error, {:credential_generation_failed, error}}
  end

  @doc "Listen for one bounded promoted connection."
  @spec listen(pid(), keyword()) :: {:ok, pid(), :inet.port_number()} | {:error, term()}
  def listen(owner, opts) when is_pid(owner) and is_list(opts) do
    Listener.start_link(owner, opts)
  end

  @doc "Dial and authenticate a bounded promoted connection."
  @spec connect(pid(), keyword()) :: {:ok, pid()} | {:error, term()}
  def connect(owner, opts) when is_pid(owner) and is_list(opts) do
    with :ok <- validate_common(owner, opts),
         {:ok, host} <- host(Keyword.get(opts, :host)),
         {:ok, port} <- port(Keyword.get(opts, :port)) do
      case :gen_tcp.connect(host, port, tcp_options(host), timeout(opts)) do
        {:ok, tcp} ->
          case :ssl.connect(tcp, tls_options(opts, :client), timeout(opts)) do
            {:ok, ssl} ->
              start_connection(owner, ssl, opts, :client)

            {:error, reason} ->
              :gen_tcp.close(tcp)
              {:error, reason}
          end

        {:error, reason} ->
          {:error, reason}
      end
    else
      {:error, _} = error -> error
    end
  end

  @doc "Attempt one bounded, relay-coordinated TCP simultaneous open."
  @spec punch(pid(), keyword()) :: {:ok, pid()} | {:error, term()}
  def punch(owner, opts) when is_pid(owner) and is_list(opts) do
    with :ok <- validate_common(owner, opts),
         {:ok, host} <- host(Keyword.get(opts, :host)),
         {:ok, port} <- port(Keyword.get(opts, :port)),
         :ok <- Punch.validate(opts, host, port) do
      Punch.run(owner, opts)
    else
      {:error, _} = error -> error
    end
  end

  @doc "Send one ARC packet, with a strict carrier frame limit."
  @spec send_packet(pid(), binary()) :: :ok | {:error, term()}
  def send_packet(pid, packet)
      when is_pid(pid) and is_binary(packet) and byte_size(packet) <= @max_packet_bytes do
    send_packet(pid, packet, System.monotonic_time(:millisecond) + @default_timeout_ms)
  end

  def send_packet(_pid, _packet), do: {:error, :packet_too_large}

  @doc "Send a packet only while the absolute monotonic lease deadline remains valid."
  @spec send_packet(pid(), binary(), integer()) :: :ok | {:error, term()}
  def send_packet(pid, packet, deadline_ms)
      when is_pid(pid) and is_binary(packet) and byte_size(packet) <= @max_packet_bytes and
             is_integer(deadline_ms) do
    remaining = deadline_ms - System.monotonic_time(:millisecond)

    if remaining > 0 do
      GenServer.call(
        pid,
        {:arc_direct_send_packet, packet, deadline_ms},
        min(remaining, @default_timeout_ms) + 100
      )
    else
      {:error, :lease_expired}
    end
  catch
    :exit, _ -> {:error, :direct_unavailable}
  end

  def send_packet(_pid, _packet, _deadline_ms), do: {:error, :packet_too_large}

  @doc "Close a direct connection or listener."
  @spec close(pid()) :: :ok
  def close(pid) when is_pid(pid) do
    if Process.alive?(pid) do
      try do
        GenServer.call(pid, :arc_direct_close, 1_000)
      catch
        :exit, _ -> :ok
      end
    end

    :ok
  end

  @doc false
  def accept(owner, socket, opts), do: start_connection(owner, socket, opts, :server)

  @doc false
  def tls_server(socket, opts),
    do: :ssl.handshake(socket, tls_options(opts, :server), timeout(opts))

  @doc false
  def validate_common(owner, opts) do
    with true <- Process.alive?(owner) or {:error, :invalid_owner},
         %Arc.Identity{} = identity <- Keyword.get(opts, :identity),
         true <- valid32?(identity.public_key) or {:error, :invalid_identity},
         %{cert_der: cert, private_key: key, fingerprint: fingerprint} <-
           Keyword.get(opts, :credentials),
         true <-
           (is_binary(cert) and not is_nil(key) and valid32?(fingerprint)) or
             {:error, :invalid_credentials},
         true <- valid32?(Keyword.get(opts, :peer_key)) or {:error, :invalid_peer_key},
         true <-
           valid32?(Keyword.get(opts, :peer_fingerprint)) or {:error, :invalid_peer_fingerprint},
         true <- valid32?(Keyword.get(opts, :binding)) or {:error, :invalid_binding},
         true <- not is_nil(Keyword.get(opts, :tag)) or {:error, :invalid_tag},
         true <- valid_timeout?(timeout(opts)) or {:error, :invalid_timeout} do
      :ok
    else
      false -> {:error, :invalid_options}
      {:error, _} = error -> error
      _ -> {:error, :invalid_options}
    end
  end

  @doc false
  def tcp_options(address \\ nil) do
    family = if is_tuple(address) and tuple_size(address) == 8, do: [:inet6], else: []
    family ++ [:binary, packet: :raw, active: false, nodelay: true, keepalive: true]
  end

  @doc false
  def timeout(opts), do: Keyword.get(opts, :timeout_ms, @default_timeout_ms)

  @doc false
  def max_packet_bytes, do: @max_packet_bytes

  @doc false
  def redact_opts(opts) do
    opts
    |> Keyword.update(:identity, nil, fn
      %Arc.Identity{public_key: public_key} -> %{public_key: public_key}
      _ -> :redacted
    end)
    |> Keyword.update(:credentials, nil, fn _ -> :redacted end)
  end

  @doc false
  def tls_options(opts, role) do
    credentials = Keyword.fetch!(opts, :credentials)
    fingerprint = Keyword.fetch!(opts, :peer_fingerprint)

    base = [
      cert: credentials.cert_der,
      key: credentials.private_key,
      verify: :verify_peer,
      # OTP requires a non-empty trust store with verify_peer. The callback
      # below admits only the relay-pinned peer leaf as this route's trust
      # anchor; this local certificate cannot trust a distinct peer.
      cacerts: [credentials.cert_der],
      verify_fun: {&verify_peer_certificate/3, fingerprint},
      versions: [:"tlsv1.3"],
      active: false,
      mode: :binary,
      packet: 4,
      packet_size: @max_packet_bytes
    ]

    if role == :server, do: [{:fail_if_no_peer_cert, true} | base], else: base
  end

  # The negotiated relay context pins the exact DER certificate. OTP still
  # performs the TLS certificate validity checks. Its generated test chain has
  # a private ephemeral root, so accept that otherwise-untrusted issuer and
  # require the exact negotiated leaf at :valid_peer. This never accepts an
  # arbitrary peer certificate or uses verify_none.
  @doc false
  def verify_peer_certificate(_cert, {:bad_cert, :unknown_ca}, expected), do: {:valid, expected}

  def verify_peer_certificate(_cert, {:bad_cert, reason}, _expected),
    do: {:fail, {:certificate_invalid, reason}}

  def verify_peer_certificate(cert, :valid_peer, expected) do
    if fingerprint(cert) == expected,
      do: {:valid, expected},
      else: {:fail, :certificate_pin_mismatch}
  end

  def verify_peer_certificate(_cert, :valid, expected), do: {:valid, expected}
  def verify_peer_certificate(_cert, _event, expected), do: {:unknown, expected}

  @doc false
  def start_connection(owner, socket, opts, role) do
    case Connection.start_link(owner: owner, socket: socket, opts: opts, role: role) do
      {:ok, pid} ->
        with :ok <- :ssl.controlling_process(socket, pid),
             :ok <- Connection.authenticate(pid, timeout(opts)) do
          {:ok, pid}
        else
          {:error, reason} ->
            _ = close(pid)
            :ssl.close(socket)
            {:error, reason}
        end

      {:error, reason} ->
        :ssl.close(socket)
        {:error, reason}
    end
  end

  defp host(host) when is_tuple(host), do: {:ok, host}
  defp host(_), do: {:error, :invalid_host}
  defp port(port) when is_integer(port) and port in 1..65_535, do: {:ok, port}
  defp port(_), do: {:error, :invalid_port}
  defp valid32?(value), do: is_binary(value) and byte_size(value) == 32
  defp valid_timeout?(value), do: is_integer(value) and value in 100..30_000

  defp fingerprint(cert) when is_binary(cert), do: :crypto.hash(:sha256, cert)

  defp fingerprint(cert) do
    :crypto.hash(:sha256, :public_key.pkix_encode(:OTPCertificate, cert, :otp))
  rescue
    _ -> <<>>
  end
end
