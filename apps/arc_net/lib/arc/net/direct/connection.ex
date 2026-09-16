defmodule Arc.Net.Direct.Connection do
  @moduledoc false
  use GenServer

  alias Arc.Identity
  alias Arc.Net.Direct

  @hello 1
  @finish 2
  @max_owner_queue 16

  def start_link(opts), do: GenServer.start(__MODULE__, opts)

  def authenticate(pid, timeout), do: GenServer.call(pid, :authenticate, timeout + 500)

  def send_packet(pid, packet, deadline_ms),
    do: GenServer.call(pid, {:arc_direct_send_packet, packet, deadline_ms}, 5_100)

  def close(pid) do
    GenServer.call(pid, :arc_direct_close, 1_000)
  catch
    :exit, _ -> :ok
  end

  @impl true
  def init(opts) do
    owner = Keyword.fetch!(opts, :owner)
    socket = Keyword.fetch!(opts, :socket)
    direct_opts = Keyword.fetch!(opts, :opts)
    role = Keyword.fetch!(opts, :role)
    Process.flag(:trap_exit, true)

    identity = Keyword.fetch!(direct_opts, :identity)
    binding = Keyword.fetch!(direct_opts, :binding)

    case Registry.register(Arc.Net.DirectRegistry, {identity.public_key, binding}, nil) do
      {:ok, _} ->
        {:ok,
         %{
           owner: owner,
           owner_ref: Process.monitor(owner),
           socket: socket,
           opts: direct_opts,
           tag: Keyword.fetch!(direct_opts, :tag),
           role: role,
           ready?: false,
           close_reason: nil
         }}

      {:error, {:already_registered, _pid}} ->
        {:stop, :binding_replayed}
    end
  end

  @impl true
  def handle_call(:authenticate, _from, %{ready?: false} = state) do
    case prove(state.socket, state.opts) do
      {:ok, peer_nonce} ->
        state = state |> Map.put(:peer_nonce, peer_nonce) |> Map.put(:ready?, true)
        send(state.owner, {:arc_direct_connected, state.tag, self()})
        {:reply, :ok, state}

      {:error, reason} ->
        {:stop, :normal, {:error, reason}, %{state | close_reason: reason}}
    end
  end

  def handle_call(:authenticate, _from, state), do: {:reply, :ok, state}

  def handle_call(:arc_direct_close, _from, state),
    do: {:stop, :normal, :ok, %{state | close_reason: :closed_by_owner}}

  def handle_call({:arc_direct_send_packet, packet, deadline_ms}, _from, state) do
    remaining = deadline_ms - System.monotonic_time(:millisecond)

    if remaining <= 0 do
      {:reply, {:error, :lease_expired}, state}
    else
      timeout = min(remaining, Direct.timeout(state.opts))
      :ok = :ssl.setopts(state.socket, send_timeout: timeout, send_timeout_close: true)
      reply = :ssl.send(state.socket, packet)

      if reply != :ok do
        {:stop, :normal, reply, %{state | close_reason: reply}}
      else
        {:reply, :ok, state}
      end
    end
  end

  @impl true
  def handle_info({:ssl, socket, packet}, %{socket: socket, ready?: true} = state) do
    if owner_queue_available?(state.owner) do
      send(state.owner, {:arc_direct_packet, state.tag, self(), packet})
      :ok = :ssl.setopts(socket, active: :once)
      {:noreply, state}
    else
      {:stop, :normal, %{state | close_reason: :owner_overloaded}}
    end
  end

  def handle_info({:ssl_closed, socket}, %{socket: socket} = state),
    do: {:stop, :normal, %{state | close_reason: :closed}}

  def handle_info({:ssl_error, socket, reason}, %{socket: socket} = state),
    do: {:stop, :normal, %{state | close_reason: reason}}

  def handle_info({:DOWN, ref, :process, _pid, _reason}, %{owner_ref: ref} = state),
    do: {:stop, :normal, %{state | close_reason: :owner_down}}

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(reason, state) do
    notify_closed(state, Map.get(state, :close_reason) || reason)
    _ = :ssl.close(state.socket)
    :ok
  end

  @impl true
  def format_status(_opt, [_pdict, state]),
    do: [data: [{~c"State", %{state | opts: Direct.redact_opts(state.opts), socket: :redacted}}]]

  defp prove(socket, opts) do
    identity = Keyword.fetch!(opts, :identity)
    own_fingerprint = opts |> Keyword.fetch!(:credentials) |> Map.fetch!(:fingerprint)
    own_nonce = :crypto.strong_rand_bytes(32)
    timeout = Direct.timeout(opts)

    with :ok <- verify_peer_pin(socket, opts),
         {:ok, exporter} <- exporter(socket, opts),
         :ok <- :ssl.send(socket, hello(identity, opts, own_fingerprint, own_nonce, exporter)),
         {:ok, peer_key, peer_nonce, peer_fingerprint} <-
           receive_hello(socket, opts, exporter, timeout),
         :ok <-
           :ssl.send(
             socket,
             finish(
               identity,
               opts,
               own_fingerprint,
               own_nonce,
               peer_key,
               peer_nonce,
               peer_fingerprint,
               exporter
             )
           ),
         :ok <-
           receive_finish(
             socket,
             opts,
             own_nonce,
             peer_key,
             peer_nonce,
             peer_fingerprint,
             exporter,
             timeout
           ),
         :ok <- :ssl.setopts(socket, active: :once) do
      {:ok, peer_nonce}
    else
      {:error, _} = error -> error
      other -> {:error, other}
    end
  end

  defp hello(identity, opts, fingerprint, nonce, exporter) do
    binding = Keyword.fetch!(opts, :binding)
    body = <<@hello, identity.public_key::binary, nonce::binary, fingerprint::binary>>
    signature = Identity.sign(identity, "arc-direct-hello-v1" <> exporter <> binding <> body)
    body <> signature
  end

  defp receive_hello(socket, opts, exporter, timeout) do
    case :ssl.recv(socket, 0, timeout) do
      {:ok,
       <<@hello, peer_key::binary-size(32), peer_nonce::binary-size(32),
         peer_fingerprint::binary-size(32), signature::binary-size(64)>>} ->
        binding = Keyword.fetch!(opts, :binding)
        body = <<@hello, peer_key::binary, peer_nonce::binary, peer_fingerprint::binary>>

        with true <- peer_key == Keyword.fetch!(opts, :peer_key) or {:error, :peer_key_mismatch},
             true <-
               peer_fingerprint == Keyword.fetch!(opts, :peer_fingerprint) or
                 {:error, :certificate_pin_mismatch},
             true <-
               Identity.verify(
                 peer_key,
                 "arc-direct-hello-v1" <> exporter <> binding <> body,
                 signature
               ) or
                 {:error, :invalid_peer_proof} do
          {:ok, peer_key, peer_nonce, peer_fingerprint}
        else
          {:error, _} = error -> error
        end

      {:ok, _} ->
        {:error, :invalid_hello}

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp finish(
         identity,
         opts,
         own_fingerprint,
         own_nonce,
         peer_key,
         peer_nonce,
         peer_fingerprint,
         exporter
       ) do
    binding = Keyword.fetch!(opts, :binding)

    body =
      <<@finish, identity.public_key::binary, own_nonce::binary, peer_key::binary,
        peer_nonce::binary, own_fingerprint::binary, peer_fingerprint::binary>>

    body <> Identity.sign(identity, "arc-direct-finish-v1" <> exporter <> binding <> body)
  end

  defp receive_finish(
         socket,
         opts,
         own_nonce,
         peer_key,
         peer_nonce,
         peer_fingerprint,
         exporter,
         timeout
       ) do
    case :ssl.recv(socket, 0, timeout) do
      {:ok,
       <<@finish, ^peer_key::binary-size(32), ^peer_nonce::binary-size(32),
         my_key::binary-size(32), ^own_nonce::binary-size(32), ^peer_fingerprint::binary-size(32),
         my_fingerprint::binary-size(32), signature::binary-size(64)>>} ->
        identity = Keyword.fetch!(opts, :identity)
        own_fingerprint = opts |> Keyword.fetch!(:credentials) |> Map.fetch!(:fingerprint)
        binding = Keyword.fetch!(opts, :binding)

        body =
          <<@finish, peer_key::binary, peer_nonce::binary, my_key::binary, own_nonce::binary,
            peer_fingerprint::binary, my_fingerprint::binary>>

        with true <- my_key == identity.public_key or {:error, :identity_proof_misdirected},
             true <- my_fingerprint == own_fingerprint or {:error, :certificate_proof_misdirected},
             true <-
               Identity.verify(
                 peer_key,
                 "arc-direct-finish-v1" <> exporter <> binding <> body,
                 signature
               ) or
                 {:error, :invalid_peer_proof} do
          :ok
        else
          {:error, _} = error -> error
        end

      {:ok, _} ->
        {:error, :invalid_finish}

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp notify_closed(state, reason) do
    send(state.owner, {:arc_direct_closed, state.tag, self(), reason})
  end

  defp owner_queue_available?(owner) do
    case Process.info(owner, :message_queue_len) do
      {:message_queue_len, count} when count < @max_owner_queue -> true
      _ -> false
    end
  end

  defp verify_peer_pin(socket, opts) do
    with {:ok, cert} <- :ssl.peercert(socket),
         expected <- Keyword.fetch!(opts, :peer_fingerprint),
         true <- :crypto.hash(:sha256, cert) == expected or {:error, :certificate_pin_mismatch} do
      :ok
    else
      {:error, _} = error -> error
      _ -> {:error, :certificate_pin_mismatch}
    end
  end

  defp exporter(socket, opts) do
    binding = Keyword.fetch!(opts, :binding)

    case :ssl.export_key_materials(socket, ["EXPORTER-ARC-DIRECT-V1"], [binding], [32]) do
      {:ok, [bytes]} when is_binary(bytes) and byte_size(bytes) == 32 -> {:ok, bytes}
      _ -> {:error, :exporter_unavailable}
    end
  catch
    :exit, _ -> {:error, :exporter_unavailable}
  end
end
