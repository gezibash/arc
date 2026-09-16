defmodule Arc.Host.Service do
  @moduledoc """
  Local ARC host service for SDK-style consumers.

  The host is a user-scoped, unprivileged runtime reachable over a Unix domain
  socket. Clients initialize against one local ARC identity, then use the host
  to resolve peers, discover capabilities, fetch manifests, send messages, and
  invoke remote capabilities without reimplementing ARC protocol machinery.
  """

  use GenServer

  alias Arc.Host.AgentPool
  alias Arc.Host.Connection
  alias Arc.Host.Protocol
  alias Arc.Host.Token

  @default_socket_path Path.join(["~", ".config", "arc", "host.sock"])

  def start_link(opts \\ []) do
    GenServer.start_link(__MODULE__, opts)
  end

  def status(server) when is_pid(server) do
    GenServer.call(server, :status)
  end

  def shutdown(server) when is_pid(server) do
    GenServer.cast(server, :shutdown)
  end

  def issue_token(server, admin_token, opts) when is_pid(server) and is_binary(admin_token) do
    GenServer.call(server, {:issue_token, admin_token, opts}, 15_000)
  end

  def authenticate_token(server, token) when is_pid(server) and is_binary(token) do
    GenServer.call(server, {:authenticate_token, token}, 15_000)
  end

  def default_socket_path do
    Application.get_env(:arc_data, :host_socket_path, @default_socket_path)
    |> Path.expand()
  end

  def admin_token_path(socket_path \\ default_socket_path()) do
    socket_path
    |> Path.expand()
    |> Path.rootname(".sock")
    |> Kernel.<>(".token")
  end

  def read_admin_token(socket_path \\ default_socket_path()) do
    socket_path
    |> admin_token_path()
    |> File.read()
    |> case do
      {:ok, token} ->
        token
        |> String.trim()
        |> case do
          "" -> {:error, :missing}
          value -> {:ok, value}
        end

      {:error, reason} ->
        {:error, reason}
    end
  end

  @impl GenServer
  def init(opts) do
    Process.flag(:trap_exit, true)

    socket_path = Keyword.get(opts, :socket_path, default_socket_path()) |> Path.expand()
    admin_token_path = Keyword.get(opts, :admin_token_path, admin_token_path(socket_path))
    File.mkdir_p!(Path.dirname(socket_path))
    File.mkdir_p!(Path.dirname(admin_token_path))
    _ = File.rm(socket_path)
    _ = File.rm(admin_token_path)

    relay = Keyword.get(opts, :relay)
    relay_pubkey = Keyword.get(opts, :relay_pubkey)

    {:ok, pool} = AgentPool.start_link(relay: relay, relay_pubkey: relay_pubkey)
    {admin_token, admin_record} = Token.generate_admin(label: "arc-host-admin")
    :ok = write_token_file(admin_token_path, admin_token)

    owner = self()

    with {:ok, listener} <- open_listener(socket_path),
         {:ok, acceptor} <- Task.start_link(fn -> accept_loop(listener, owner) end) do
      acceptor_ref = Process.monitor(acceptor)

      {:ok,
       %{
         socket_path: socket_path,
         listener: listener,
         agent_pool: pool,
         acceptor: acceptor,
         acceptor_ref: acceptor_ref,
         connections: %{},
         admin_token_path: admin_token_path,
         tokens: %{admin_record.hash => admin_record},
         relay: relay,
         relay_pubkey: relay_pubkey,
         started_at: DateTime.utc_now()
       }}
    else
      {:error, reason} ->
        {:stop, reason}
    end
  end

  @impl GenServer
  def handle_call(:status, _from, state) do
    pool = AgentPool.status(state.agent_pool)

    result = %{
      "socket_path" => state.socket_path,
      "admin_token_path" => state.admin_token_path,
      "protocol_version" => Protocol.version(),
      "started_at" => DateTime.to_iso8601(state.started_at),
      "connections" => map_size(state.connections),
      "identity_count" => pool.identity_count,
      "identities" => pool.identities,
      "relay" => relay_document(state.relay, state.relay_pubkey),
      "relay_connections" => pool.relay_connections
    }

    {:reply, result, state}
  end

  def handle_call({:authenticate_token, token}, _from, state) do
    state = prune_expired_tokens(state)

    case fetch_token(state, token) do
      {:ok, record} ->
        {:reply, {:ok, record}, state}

      {:error, reason} ->
        {:reply, {:error, reason}, state}
    end
  end

  def handle_call({:issue_token, admin_token, opts}, _from, state) do
    state = prune_expired_tokens(state)

    with {:ok, record} <- fetch_token(state, admin_token),
         true <- record.kind == :admin or {:error, :forbidden},
         {:ok, identity} <- resolve_issue_identity(opts),
         scopes <- Token.normalize_scopes(Keyword.get(opts, :scopes, Token.delegated_scopes())),
         true <- scopes != [] or {:error, :invalid_scope},
         {raw_token, issued_record} <-
           Token.generate_delegated(
             identity: identity,
             scopes: scopes,
             ttl_seconds: normalize_ttl(Keyword.get(opts, :ttl_seconds)),
             label: normalize_label(Keyword.get(opts, :label))
           ) do
      issued = Token.document(issued_record, raw_token)
      state = put_in(state.tokens[issued_record.hash], issued_record)
      {:reply, {:ok, issued}, state}
    else
      {:error, reason} ->
        {:reply, {:error, reason}, state}
    end
  end

  @impl GenServer
  def handle_cast(:shutdown, state) do
    {:stop, :normal, state}
  end

  @impl GenServer
  def handle_info({:accepted, socket}, state) do
    {:ok, pid} =
      Connection.start_link(
        socket: socket,
        service: self(),
        agent_pool: state.agent_pool
      )

    ref = Process.monitor(pid)
    {:noreply, put_in(state.connections[ref], pid)}
  end

  def handle_info({:accept_error, :closed}, state), do: {:noreply, state}
  def handle_info({:accept_error, _reason}, state), do: {:noreply, state}

  def handle_info({:DOWN, ref, :process, _pid, _reason}, %{acceptor_ref: ref} = state) do
    {:stop, :acceptor_down, state}
  end

  def handle_info({:DOWN, ref, :process, _pid, _reason}, state) do
    {:noreply, %{state | connections: Map.delete(state.connections, ref)}}
  end

  def handle_info(_msg, state), do: {:noreply, state}

  @impl GenServer
  def terminate(_reason, state) do
    Enum.each(state.connections, fn {_ref, pid} ->
      if Process.alive?(pid), do: Process.exit(pid, :normal)
    end)

    if is_pid(state.acceptor), do: Process.exit(state.acceptor, :normal)
    if is_pid(state.agent_pool), do: Process.exit(state.agent_pool, :normal)
    _ = :socket.close(state.listener)
    _ = File.rm(state.socket_path)
    _ = File.rm(state.admin_token_path)
    :ok
  end

  defp open_listener(socket_path) do
    with {:ok, listener} <- :socket.open(:local, :stream, :default),
         :ok <- :socket.bind(listener, %{family: :local, path: String.to_charlist(socket_path)}),
         :ok <- :socket.listen(listener),
         :ok <- File.chmod(socket_path, 0o600) do
      {:ok, listener}
    end
  end

  defp accept_loop(listener, owner) do
    case :socket.accept(listener) do
      {:ok, socket} ->
        send(owner, {:accepted, socket})
        accept_loop(listener, owner)

      {:error, reason} ->
        send(owner, {:accept_error, reason})
        :ok
    end
  end

  defp write_token_file(path, token) do
    :ok = File.write(path, token <> "\n")
    File.chmod(path, 0o600)
  end

  defp fetch_token(state, token) when is_binary(token) do
    case Map.get(state.tokens, Token.hash(token)) do
      nil ->
        {:error, :unauthorized}

      record ->
        if Token.expired?(record), do: {:error, :expired}, else: {:ok, record}
    end
  end

  defp fetch_token(_state, _token), do: {:error, :unauthorized}

  defp prune_expired_tokens(state) do
    tokens =
      Enum.reduce(state.tokens, %{}, fn {hash, record}, acc ->
        if Token.expired?(record), do: acc, else: Map.put(acc, hash, record)
      end)

    %{state | tokens: tokens}
  end

  defp resolve_issue_identity(opts) do
    case Keyword.get(opts, :identity) do
      %Arc.Identity{} = identity ->
        {:ok, identity}

      nil ->
        {:error, :identity_required}

      query ->
        AgentPool.resolve_identity(query)
    end
  end

  defp normalize_ttl(value) when is_integer(value) and value > 0, do: value

  defp normalize_ttl(value) when is_binary(value) do
    case Integer.parse(String.trim(value)) do
      {ttl, ""} when ttl > 0 -> ttl
      _ -> nil
    end
  end

  defp normalize_ttl(_value), do: nil

  defp normalize_label(value) when is_binary(value) do
    case String.trim(value) do
      "" -> nil
      label -> label
    end
  end

  defp normalize_label(_value), do: nil

  defp relay_document(nil, _pubkey), do: nil

  defp relay_document({host, port}, pubkey) do
    %{
      "host" => List.to_string(host),
      "port" => port,
      "pubkey" => if(is_binary(pubkey), do: Base.encode16(pubkey, case: :lower), else: nil)
    }
  end
end
