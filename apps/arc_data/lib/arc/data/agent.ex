defmodule Arc.Data.Agent do
  @moduledoc """
  The universal agent. A BEAM process IS an agent.

  Each agent has its own keypair, petname, and session state.
  Agents publish themselves to the control plane, establish
  encrypted sessions with peers, and send/receive messages.

  An agent can optionally have a handler (via `arc serve <uri>`)
  which processes incoming messages and sends replies.
  """

  use GenServer
  require Logger

  alias Arc.Control
  alias Arc.Data.CapabilityManifest
  alias Arc.Data.CapabilityPackage
  alias Arc.Data.Direct
  alias Arc.Data.Frame
  alias Arc.Data.Handler
  alias Arc.Data.Mailbox
  alias Arc.Data.Packet
  alias Arc.Data.RelayAnnouncement
  alias Arc.Data.Session
  alias Arc.Identity

  @default_allowed_clock_skew_ms 120_000
  @request_route_sweep_ms 1_000
  @max_pending_routes 1_024
  @max_request_deadline_ms 120_000
  @refresh_lookup_timeout_ms 750
  @max_refresh_workers 32

  @type t :: %{
          identity: Identity.t(),
          sessions: %{binary() => Session.t()},
          inbox: [map()],
          handler: {module(), term()} | nil,
          observer: pid() | nil,
          replay_guard: %{
            {binary(), binary()} => %{max_seq: non_neg_integer(), max_ts: integer()}
          },
          allowed_clock_skew_ms: pos_integer()
        }

  # --- Client API ---

  def start_link(%Identity{} = identity, opts \\ []) do
    {handler_uri, opts} = Keyword.pop(opts, :serve)
    {observer, opts} = Keyword.pop(opts, :observer)
    {direct_policy, opts} = Keyword.pop(opts, :direct_policy, [])
    GenServer.start_link(__MODULE__, {identity, handler_uri, observer, direct_policy}, opts)
  end

  @doc """
  Send an event frame to a peer outside any request. The peer sees it in
  its inbox with kind `:event` and `meta["topic"]`.
  """
  @spec emit_event(GenServer.server(), binary(), String.t(), binary()) :: :ok
  def emit_event(agent, <<to_pk::binary-size(32)>>, topic, body)
      when is_binary(topic) and is_binary(body) do
    GenServer.call(agent, {:emit_event, to_pk, topic, body})
  end

  def publish(agent) do
    GenServer.call(agent, :publish)
  end

  @doc "Announce this identity and its services through its connected relay."
  def publish_relay(agent, opts \\ []) when is_list(opts) do
    GenServer.call(agent, {:publish_relay, opts}, 5_000)
  end

  def connect(agent, peer_query, timeout \\ 11_000) when is_binary(peer_query) do
    # Relay directory lookup may traverse several bounded partner branches.
    # Keep the caller alive through the transport's ten-second lookup deadline.
    GenServer.call(agent, {:connect, peer_query}, timeout)
  end

  def send_message(agent, peer_query, message) when is_binary(message) do
    send_message(agent, peer_query, message, [])
  end

  def send_message(agent, peer_query, message, opts)
      when is_binary(message) and is_list(opts) do
    timeout =
      case Keyword.get(opts, :deadline_ms) do
        deadline when is_integer(deadline) ->
          max(1, min(5_000, deadline - System.monotonic_time(:millisecond)))

        _ ->
          5_000
      end

    GenServer.call(agent, {:send, peer_query, message, opts}, timeout)
  end

  @doc "Return the optional direct-route manager for this agent."
  @spec direct(GenServer.server()) :: pid() | nil
  def direct(agent), do: GenServer.call(agent, :direct)

  @doc "Forget a request route after the caller's deadline; it is never replayed."
  @spec finish_request(GenServer.server(), binary(), binary()) :: :ok
  def finish_request(agent, <<peer::binary-size(32)>>, <<request_id::binary-size(16)>>) do
    GenServer.cast(agent, {:finish_request, peer, request_id})
  end

  def read_inbox(agent) do
    GenServer.call(agent, :read_inbox)
  end

  def take_inbox(agent, matcher) when is_function(matcher, 1) do
    GenServer.call(agent, {:take_inbox, matcher})
  end

  def poll_mailbox(agent) do
    GenServer.cast(agent, :poll_mailbox)
  end

  def info(agent) do
    GenServer.call(agent, :info)
  end

  @doc "Sign a public Agora post as this local agent without exporting its key."
  def sign_agora_post(agent, board, body, parent) do
    GenServer.call(agent, {:sign_agora_post, board, body, parent})
  end

  # --- GenServer Callbacks ---

  @impl GenServer
  def init({%Identity{} = identity, handler_uri, observer, direct_policy}) do
    with {:ok, _} <- Registry.register(Arc.Data.AgentRegistry, identity.public_key, []),
         {:ok, handler} <- init_handler(handler_uri),
         {:ok, direct} <- init_direct(identity, direct_policy) do
      state = %{
        identity: identity,
        sessions: %{},
        inbox: [],
        handler: handler,
        observer: observer,
        relay_discovery: false,
        relay_federation: :local,
        announcement_timer: nil,
        announcement_generation: nil,
        direct: direct,
        refresh_workers: %{},
        refresh_workers_by_peer: %{},
        request_routes: %{},
        expired_direct_reply_routes: %{},
        direct_freezes: %{},
        replay_guard: %{},
        allowed_clock_skew_ms:
          Application.get_env(:arc_data, :allowed_clock_skew_ms, @default_allowed_clock_skew_ms)
      }

      schedule_replay_sweep(state)
      schedule_request_route_sweep()
      {:ok, state}
    else
      {:error, {:already_registered, _pid}} ->
        {:stop, {:already_registered, identity.public_key}}

      {:error, reason} ->
        {:stop, reason}
    end
  end

  @impl GenServer
  def handle_call(:publish, _from, state) do
    id = state.identity
    result = Control.publish(id)

    case result do
      :ok ->
        {x_pub, _x_priv} = Identity.to_x25519(id)
        Control.publish_keyex(id.public_key, x_pub)
        {:reply, :ok, state}

      error ->
        {:reply, error, state}
    end
  end

  def handle_call({:publish_relay, opts}, _from, state) do
    # Once chosen, relay mode stays selected even if announcement or transport
    # fails. A discovery failure must not switch the caller to local records.
    federation = Keyword.get(opts, :federation, :local)
    state = %{state | relay_discovery: true, relay_federation: federation}
    result = announce_relay(state)
    {:reply, result, schedule_announcement(state)}
  end

  def handle_call({:connect, peer_query}, _from, state) do
    case resolve_entry(state, peer_query) do
      {:ok, [entry]} ->
        if entry.x25519_public do
          case establish_session(state.identity, entry, &Session.establish/3) do
            {:ok, session} ->
              sessions = Map.put(state.sessions, entry.public_key, session)
              {:reply, {:ok, entry}, %{state | sessions: sessions}}

            {:error, reason} ->
              {:reply, {:error, reason}, state}
          end
        else
          {:reply, {:error, :no_keyex}, state}
        end

      {:ok, []} ->
        {:reply, {:error, :not_found}, state}

      {:ok, [_ | _]} ->
        {:reply, {:error, :ambiguous}, state}

      error ->
        {:reply, error, state}
    end
  end

  def handle_call({:emit_event, to_pk, topic, body}, _from, state) do
    state = send_reply(state, to_pk, Frame.encode_event(topic, body))
    {:reply, :ok, state}
  end

  def handle_call({:send, peer_query, message, opts}, _from, state) do
    with {:ok, peer_pk} <- resolve_peer_key(state, peer_query),
         :ok <- allow_outgoing_request(state, peer_pk, opts),
         %Session{} = session <- Map.get(state.sessions, peer_pk) do
      opts = ensure_request_id(opts)
      payload = build_outgoing_payload(message, opts)
      send_outgoing_payload(state, peer_pk, payload, opts, session)
    else
      nil -> {:reply, {:error, :no_session}, state}
      error -> {:reply, error, state}
    end
  end

  def handle_call(:read_inbox, _from, state) do
    {:reply, Enum.reverse(state.inbox), %{state | inbox: []}}
  end

  def handle_call(:direct, _from, state), do: {:reply, state.direct, state}

  def handle_call({:take_inbox, matcher}, _from, state) when is_function(matcher, 1) do
    messages = Enum.reverse(state.inbox)

    case take_first_matching(messages, matcher, []) do
      {:ok, match, remaining} ->
        {:reply, [match], %{state | inbox: Enum.reverse(remaining)}}

      :error ->
        {:reply, [], state}
    end
  end

  def handle_call(:info, _from, state) do
    {handler_scheme, _} =
      if state.handler do
        {mod, _} = state.handler
        {mod |> Module.split() |> List.last() |> String.downcase(), true}
      else
        {nil, false}
      end

    info = %{
      name: Identity.name(state.identity),
      short_name: Identity.short_name(state.identity),
      public_key: state.identity.public_key,
      sessions: Map.keys(state.sessions) |> length(),
      serving: handler_scheme,
      relay_discovery: state.relay_discovery,
      relay_federation: state.relay_federation
    }

    {:reply, info, state}
  end

  @impl GenServer
  def handle_call({:sign_agora_post, board, body, parent}, _from, state) do
    {:reply, Arc.Data.Agora.sign(state.identity, board, body, parent), state}
  end

  @impl GenServer
  def handle_cast({:finish_request, peer, request_id}, state) do
    {:noreply, %{state | request_routes: Map.delete(state.request_routes, {peer, request_id})}}
  end

  def handle_cast(:poll_mailbox, %{relay_discovery: true} = state), do: {:noreply, state}

  def handle_cast(:poll_mailbox, state) do
    packets = Mailbox.read(state.identity.public_key)
    state = Enum.reduce(packets, state, &receive_packet(&1, &2, :local))
    {:noreply, state}
  end

  @impl GenServer
  def handle_info({:arc_packet, packet}, state) do
    {:noreply, receive_packet(packet, state, :local)}
  end

  # The relay transport marks its ingress explicitly. Direct-control frames are
  # reserved to that ingress and cannot be injected by a same-host sender.
  def handle_info({:arc_relay_packet, packet}, state) do
    {:noreply, receive_packet(packet, state, :relay)}
  end

  def handle_info({:arc_relay_status, :up}, state) do
    if is_pid(state.direct), do: send(state.direct, {:arc_relay_status, :up})

    state =
      if state.relay_discovery do
        case announce_relay(state) do
          :ok -> :ok
          {:error, reason} -> Logger.warning("relay reannouncement failed: #{inspect(reason)}")
        end

        schedule_announcement(state)
      else
        state
      end

    {:noreply, state}
  end

  def handle_info({:arc_relay_status, :down}, state) do
    if is_pid(state.direct), do: send(state.direct, {:arc_relay_status, :down})
    {:noreply, state}
  end

  def handle_info({:arc_direct_refresh, manager, ref, peer_key}, %{direct: manager} = state)
      when is_reference(ref) and is_binary(peer_key) do
    {:noreply, start_direct_refresh(state, manager, ref, peer_key)}
  end

  def handle_info({:arc_direct_refresh, _manager, _ref, _peer_key}, state), do: {:noreply, state}

  def handle_info({:arc_direct_refresh_lookup, token, result}, state) do
    {:noreply, apply_direct_refresh_lookup(state, token, result)}
  end

  def handle_info({:arc_direct_refresh_timeout, token}, state) do
    {:noreply, expire_direct_refresh_worker(state, token)}
  end

  def handle_info({:DOWN, monitor, :process, _pid, _reason}, state) do
    {:noreply, remove_refresh_worker_by_monitor(state, monitor)}
  end

  def handle_info(
        {:arc_direct_control, manager, ref, peer_key, control_map},
        %{direct: manager} = state
      )
      when is_reference(ref) and is_binary(peer_key) and is_map(control_map) do
    {result, session_id, state} = send_direct_control(state, peer_key, control_map)
    send(manager, {:arc_direct_control_result, ref, result, session_id})
    {:noreply, state}
  end

  def handle_info({:arc_direct_control, _manager, _ref, _peer_key, _control_map}, state),
    do: {:noreply, state}

  def handle_info(
        {:arc_direct_validate, manager, ref, capability_id, package_hash},
        %{direct: manager} = state
      )
      when is_reference(ref) and is_binary(capability_id) do
    result = validate_direct_capability(state, capability_id, package_hash)
    send(manager, {:arc_direct_validated, ref, result})
    {:noreply, state}
  end

  def handle_info({:arc_direct_validate, _manager, _ref, _capability_id, _package_hash}, state),
    do: {:noreply, state}

  def handle_info({:arc_direct_prepare, manager, ref, peer_key}, %{direct: manager} = state)
      when is_reference(ref) and is_binary(peer_key) do
    if peer_has_pending_routes?(state, peer_key) do
      send(manager, {:arc_direct_prepared, ref, {:error, :busy}})
      {:noreply, state}
    else
      send(manager, {:arc_direct_prepared, ref, :ok})
      {:noreply, %{state | direct_freezes: Map.put(state.direct_freezes, peer_key, ref)}}
    end
  end

  def handle_info({:arc_direct_prepare, _manager, _ref, _peer_key}, state), do: {:noreply, state}

  def handle_info({:arc_direct_release, manager, ref, peer_key}, %{direct: manager} = state)
      when is_reference(ref) and is_binary(peer_key) do
    send(manager, {:arc_direct_released, ref, :ok})

    freezes =
      if state.direct_freezes[peer_key] == ref,
        do: Map.delete(state.direct_freezes, peer_key),
        else: state.direct_freezes

    {:noreply, %{state | direct_freezes: freezes}}
  end

  def handle_info({:arc_direct_release, _manager, _ref, _peer_key}, state), do: {:noreply, state}

  def handle_info(
        {:arc_direct_frame, manager, generation, peer_key, frame, deadline},
        %{direct: manager} = state
      )
      when is_binary(peer_key) do
    {:noreply, receive_direct_frame(state, manager, generation, peer_key, frame, deadline)}
  end

  def handle_info(
        {:arc_direct_frame, _manager, _generation, _peer_key, _frame, _deadline},
        state
      ),
      do: {:noreply, state}

  def handle_info(:sweep_replay_guard, state) do
    state = sweep_replay_guard(state, System.system_time(:millisecond))
    schedule_replay_sweep(state)
    {:noreply, state}
  end

  def handle_info(:sweep_request_routes, state) do
    {request_routes, expired_direct_reply_routes} = sweep_request_routes(state)

    state = %{
      state
      | request_routes: request_routes,
        expired_direct_reply_routes: expired_direct_reply_routes
    }

    schedule_request_route_sweep()
    {:noreply, state}
  end

  def handle_info({:refresh_relay_announcement, generation}, state)
      when generation == state.announcement_generation do
    case announce_relay(state) do
      :ok -> :ok
      {:error, reason} -> Logger.warning("relay announcement failed: #{inspect(reason)}")
    end

    {:noreply, schedule_announcement(%{state | announcement_timer: nil})}
  end

  def handle_info({:refresh_relay_announcement, _generation}, state), do: {:noreply, state}

  def handle_info(msg, %{handler: {mod, handler_state}} = state) do
    if function_exported?(mod, :handle_info, 2) do
      case mod.handle_info(msg, handler_state) do
        :unhandled ->
          {:noreply, state}

        {:noreply, new_handler_state} ->
          {:noreply, %{state | handler: {mod, new_handler_state}}}

        {:emit, events, new_handler_state} ->
          state =
            %{state | handler: {mod, new_handler_state}}
            |> emit_handler_events(events)

          {:noreply, state}
      end
    else
      {:noreply, state}
    end
  end

  def handle_info(_msg, state), do: {:noreply, state}

  @impl GenServer
  def terminate(_reason, state) do
    Enum.each(state.refresh_workers, fn {_token, worker} ->
      Process.demonitor(worker.monitor, [:flush])
      if Process.alive?(worker.pid), do: Process.exit(worker.pid, :kill)
    end)

    :ok
  end

  # --- Private ---

  @impl GenServer
  def format_status(_reason, [_process_dictionary, state]) when is_map(state) do
    [
      data: [
        {~c"State",
         %{
           identity: :redacted,
           sessions: map_size(Map.get(state, :sessions, %{})),
           inbox: length(Map.get(state, :inbox, [])),
           relay_discovery: Map.get(state, :relay_discovery, false),
           direct: is_pid(Map.get(state, :direct)),
           request_routes: map_size(Map.get(state, :request_routes, %{}))
         }}
      ]
    ]
  end

  def format_status(_reason, _state), do: [data: [{~c"State", :redacted}]]

  defp init_direct(_identity, []), do: {:ok, nil}

  defp init_direct(identity, rules) when is_list(rules) do
    Direct.start_link(identity: identity, owner: self(), policy: rules)
  end

  defp init_direct(_identity, _rules), do: {:error, :invalid_direct_policy}

  defp allow_outgoing_request(state, peer_pk, opts) do
    if Map.has_key?(state.direct_freezes, peer_pk) and
         is_nil(Keyword.get(opts, :direct_generation)) do
      {:error, :direct_preparing}
    else
      :ok
    end
  end

  defp send_outgoing_payload(state, peer_pk, payload, opts, session) do
    generation = Keyword.get(opts, :direct_generation, :relay)

    case track_outgoing_route(state, peer_pk, payload, opts, generation) do
      {:ok, state} ->
        {result, state} = send_payload_on_generation(state, peer_pk, payload, session, generation)

        state =
          if result == :ok, do: state, else: untrack_outgoing_route(state, peer_pk, payload, opts)

        {:reply, result, state}

      {:error, _reason} = error ->
        {:reply, error, state}
    end
  end

  defp send_payload_on_generation(state, _peer_pk, payload, _session, generation)
       when generation != :relay do
    case state.direct do
      pid when is_pid(pid) -> {Direct.send_packet(pid, generation, payload), state}
      _ -> {{:error, :direct_unavailable}, state}
    end
  end

  defp send_payload_on_generation(state, peer_pk, payload, session, :relay) do
    {nonce, ciphertext, seq, session} = Session.encrypt(session, payload)
    state = %{state | sessions: Map.put(state.sessions, peer_pk, session)}

    packet =
      Packet.encode(state.identity, peer_pk, session.session_id, seq, nonce, ciphertext,
        ek: session.ek_pub
      )

    # Return the advanced relay session on success or failure. Session counters
    # must never be reused after a failed carrier send.
    case deliver(state, peer_pk, packet) do
      :ok -> {:ok, state}
      {:error, _} = error -> {error, state}
    end
  end

  defp track_outgoing_route(state, peer_pk, _payload, opts, generation) do
    if Keyword.get(opts, :track_reply, false) or not Keyword.get(opts, :raw, false) do
      request_id = Keyword.get(opts, :request_id)

      with <<_::binary-size(16)>> <- request_id,
           {:ok, deadline} <- request_deadline(opts),
           true <-
             map_size(state.request_routes) < @max_pending_routes or
               {:error, :too_many_pending_requests} do
        route = %{direction: :outgoing, generation: generation, deadline: deadline}

        {:ok,
         %{state | request_routes: Map.put(state.request_routes, {peer_pk, request_id}, route)}}
      else
        _ -> {:error, :invalid_request_tracking}
      end
    else
      {:ok, state}
    end
  end

  defp untrack_outgoing_route(state, peer_pk, _payload, opts) do
    case Keyword.get(opts, :request_id) do
      <<_::binary-size(16)>> = request_id ->
        case Map.get(state.request_routes, {peer_pk, request_id}) do
          %{direction: :outgoing} ->
            %{state | request_routes: Map.delete(state.request_routes, {peer_pk, request_id})}

          _ ->
            state
        end

      _ ->
        state
    end
  end

  defp request_deadline(opts) do
    now = System.monotonic_time(:millisecond)

    case Keyword.get(opts, :deadline_ms) do
      deadline
      when is_integer(deadline) and deadline > now and deadline - now <= @max_request_deadline_ms ->
        {:ok, deadline}

      timeout when is_integer(timeout) and timeout in 1..@max_request_deadline_ms ->
        {:ok, now + timeout}

      nil ->
        {:ok, now + @max_request_deadline_ms}

      _ ->
        {:error, :invalid_request_deadline}
    end
  end

  defp track_incoming_route(state, peer_pk, request_id, generation, deadline) do
    with <<_::binary-size(16)>> <- request_id,
         {:ok, deadline} <- normalize_direct_deadline(deadline),
         false <- Map.has_key?(state.request_routes, {peer_pk, request_id}),
         true <- map_size(state.request_routes) < @max_pending_routes do
      route = %{direction: :incoming, generation: generation, deadline: deadline}

      {:ok,
       %{
         state
         | request_routes: Map.put(state.request_routes, {peer_pk, request_id}, route),
           expired_direct_reply_routes:
             Map.delete(state.expired_direct_reply_routes, {peer_pk, request_id})
       }}
    else
      true -> {:error, :duplicate_request_id}
      false -> {:error, :too_many_pending_requests}
      _ -> {:error, :invalid_request_tracking}
    end
  end

  defp normalize_direct_deadline(deadline) when is_integer(deadline) do
    now = System.monotonic_time(:millisecond)

    if deadline > now and deadline - now <= @max_request_deadline_ms,
      do: {:ok, deadline},
      else: {:error, :invalid_direct_deadline}
  end

  defp normalize_direct_deadline(nil),
    do: {:ok, System.monotonic_time(:millisecond) + @max_request_deadline_ms}

  defp normalize_direct_deadline(_), do: {:error, :invalid_direct_deadline}

  defp sweep_request_routes(state) do
    now = System.monotonic_time(:millisecond)

    {expired, active} =
      Enum.split_with(state.request_routes, fn {_key, %{deadline: deadline}} ->
        deadline <= now
      end)

    tombstones =
      Enum.reduce(expired, state.expired_direct_reply_routes, fn
        {{peer, request_id}, %{direction: :incoming, generation: generation}}, acc
        when generation != :relay ->
          Map.put(acc, {peer, request_id}, now + @max_request_deadline_ms)

        _, acc ->
          acc
      end)
      |> Map.reject(fn {_key, expires} -> expires <= now end)

    {Map.new(active), tombstones}
  end

  defp schedule_request_route_sweep do
    Process.send_after(self(), :sweep_request_routes, @request_route_sweep_ms)
  end

  defp peer_has_pending_routes?(state, peer_key) do
    Enum.any?(state.request_routes, fn {{peer, _request_id}, _route} -> peer == peer_key end)
  end

  defp receive_direct_frame(state, manager, generation, peer_key, frame, deadline) do
    if Direct.admitted?(manager, generation) do
      dispatch_direct_frame(state, peer_key, generation, frame, deadline)
    else
      state
    end
  end

  defp dispatch_direct_frame(state, peer_key, generation, %{type: type} = frame, deadline)
       when type in [:request, :stream_open, :stream_data, :stream_resize, :stream_close] do
    case track_incoming_route(state, peer_key, frame.request_id, generation, deadline) do
      {:ok, state} when not is_nil(state.handler) ->
        dispatch_handler_frame(
          frame,
          peer_key,
          direct_context_id(generation),
          true,
          state,
          generation,
          deadline
        )

      {:ok, state} ->
        append_inbox_message(encode_direct_frame(frame), peer_key, generation, state)

      {:error, _} ->
        state
    end
  end

  defp dispatch_direct_frame(state, peer_key, generation, %{type: _type} = frame, _deadline) do
    append_inbox_message(encode_direct_frame(frame), peer_key, generation, state)
  end

  defp dispatch_direct_frame(state, _peer_key, _generation, _frame, _deadline), do: state

  defp encode_direct_frame(%{type: type, request_id: request_id, meta: meta, body: body}) do
    Frame.encode_frame(type, request_id, meta, body)
  rescue
    _ -> <<>>
  end

  defp direct_context_id(generation) when is_binary(generation), do: generation
  defp direct_context_id(generation), do: :erlang.term_to_binary(generation)

  # `arc.direct.v1` belongs to ARC control. Reserve it before application
  # dispatch under every ingress condition, including local packets, a missing
  # direct manager, and malformed or oversized control bodies.
  defp maybe_direct_control(plaintext, decoded, ingress, %{direct: manager} = state) do
    case Frame.decode(plaintext) do
      {:ok, %{type: :event, meta: %{"topic" => "arc.direct.v1"}, body: body}} ->
        if ingress == :relay and is_pid(manager) and byte_size(body) <= 16_384 do
          case decode_direct_control(body) do
            {:ok, control} ->
              send(manager, {:arc_direct_control, decoded.src, decoded.session_id, control})
              {:handled, state}

            :error ->
              {:handled, state}
          end
        else
          {:handled, state}
        end

      _ ->
        :not_control
    end
  end

  defp maybe_direct_control(_plaintext, _decoded, _ingress, _state), do: :not_control

  defp decode_direct_control(body) do
    case :json.decode(body) do
      value when is_map(value) -> {:ok, value}
      _ -> :error
    end
  rescue
    _ -> :error
  end

  defp send_direct_control(%{relay_discovery: false} = state, _peer_key, _control),
    do: {{:error, :relay_not_selected}, nil, state}

  defp send_direct_control(state, peer_key, control) do
    with {:ok, body} <- encode_direct_control(control),
         %Session{} = session <- Map.get(state.sessions, peer_key) do
      payload = Frame.encode_event("arc.direct.v1", body)
      {result, state} = send_payload_on_generation(state, peer_key, payload, session, :relay)
      session_id = if result == :ok, do: session.session_id, else: nil
      {result, session_id, state}
    else
      nil -> {{:error, :no_session}, nil, state}
      {:error, _} = error -> {error, nil, state}
    end
  end

  defp encode_direct_control(control) do
    body = control |> :json.encode() |> IO.iodata_to_binary()
    if byte_size(body) <= 16_384, do: {:ok, body}, else: {:error, :control_too_large}
  rescue
    _ -> {:error, :invalid_direct_control}
  end

  defp validate_direct_capability(%{handler: nil}, _capability_id, _package_hash),
    do: {:error, :not_serving}

  defp validate_direct_capability(state, capability_id, package_hash) do
    with {:ok, detail} <- CapabilityManifest.detail(state.identity, state.handler, capability_id),
         {:ok, package} <- CapabilityPackage.verify(detail),
         true <- package["package_hash"] == package_hash or {:error, :capability_hash_mismatch} do
      {:ok, package}
    else
      {:error, _} = error -> error
      _ -> {:error, :invalid_capability_package}
    end
  end

  defp start_direct_refresh(%{relay_discovery: false} = state, manager, ref, _peer_key) do
    send(manager, {:arc_direct_refreshed, ref, {:error, :relay_not_selected}, nil})
    state
  end

  defp start_direct_refresh(state, manager, ref, peer_key) do
    cond do
      pending_relay_request?(state, peer_key) ->
        send(manager, {:arc_direct_refreshed, ref, {:error, :busy}, nil})
        state

      Map.has_key?(state.refresh_workers_by_peer, peer_key) ->
        send(manager, {:arc_direct_refreshed, ref, {:error, :busy}, nil})
        state

      map_size(state.refresh_workers) >= @max_refresh_workers ->
        send(manager, {:arc_direct_refreshed, ref, {:error, :busy}, nil})
        state

      true ->
        token = make_ref()
        owner = self()
        source_key = state.identity.public_key

        {pid, monitor} =
          spawn_monitor(fn ->
            result = refresh_directory_lookup(source_key, peer_key)
            send(owner, {:arc_direct_refresh_lookup, token, result})

            receive do
              {:arc_direct_refresh_lookup_ack, ^token} -> :ok
            after
              @refresh_lookup_timeout_ms -> :ok
            end
          end)

        timer =
          Process.send_after(
            self(),
            {:arc_direct_refresh_timeout, token},
            @refresh_lookup_timeout_ms
          )

        worker = %{
          pid: pid,
          monitor: monitor,
          timer: timer,
          manager: manager,
          effect_ref: ref,
          peer_key: peer_key
        }

        %{
          state
          | refresh_workers: Map.put(state.refresh_workers, token, worker),
            refresh_workers_by_peer: Map.put(state.refresh_workers_by_peer, peer_key, token)
        }
    end
  end

  # This worker handles only public directory output. It has neither an ARC
  # identity nor session state, so a crash cannot disclose private key material.
  defp refresh_directory_lookup(source_key, peer_key) do
    query = Base.encode16(peer_key, case: :lower)

    case relay_call(:resolve_via_relay, [source_key, query]) do
      {:ok, [entry]} -> {:ok, entry}
      {:ok, _} -> {:error, :relay_refresh_failed}
      {:error, _} = error -> error
      _ -> {:error, :relay_refresh_failed}
    end
  rescue
    _ -> {:error, :relay_refresh_failed}
  catch
    :exit, _ -> {:error, :relay_refresh_failed}
  end

  defp apply_direct_refresh_lookup(state, token, result) do
    case pop_refresh_worker(state, token) do
      {nil, state} ->
        state

      {worker, state} ->
        {reply, session_id, state} =
          if refresh_worker_current?(state, worker) and
               not pending_relay_request?(state, worker.peer_key) do
            install_refreshed_relay_session(state, worker.peer_key, result)
          else
            {{:error, :busy}, nil, state}
          end

        send(worker.pid, {:arc_direct_refresh_lookup_ack, token})
        send(worker.manager, {:arc_direct_refreshed, worker.effect_ref, reply, session_id})
        state
    end
  end

  defp expire_direct_refresh_worker(state, token) do
    case pop_refresh_worker(state, token) do
      {nil, state} ->
        state

      {worker, state} ->
        if Process.alive?(worker.pid), do: Process.exit(worker.pid, :kill)
        send(worker.manager, {:arc_direct_refreshed, worker.effect_ref, {:error, :timeout}, nil})
        state
    end
  end

  defp remove_refresh_worker_by_monitor(state, monitor) do
    case Enum.find(state.refresh_workers, fn {_token, worker} -> worker.monitor == monitor end) do
      {token, _worker} -> elem(pop_refresh_worker(state, token), 1)
      nil -> state
    end
  end

  defp pop_refresh_worker(state, token) do
    case Map.pop(state.refresh_workers, token) do
      {nil, _workers} ->
        {nil, state}

      {worker, workers} ->
        Process.cancel_timer(worker.timer)
        Process.demonitor(worker.monitor, [:flush])

        {worker,
         %{
           state
           | refresh_workers: workers,
             refresh_workers_by_peer: Map.delete(state.refresh_workers_by_peer, worker.peer_key)
         }}
    end
  end

  defp refresh_worker_current?(state, worker) do
    state.direct == worker.manager and Process.alive?(worker.manager) and
      Direct.refreshing?(worker.manager, worker.effect_ref) and
      Enum.any?(Direct.status(worker.manager), fn route ->
        route.peer == Base.encode16(worker.peer_key, case: :lower) and route.role == :caller
      end)
  catch
    :exit, _ -> false
  end

  defp install_refreshed_relay_session(state, peer_key, {:ok, entry}) do
    with true <- entry.public_key == peer_key or {:error, :peer_mismatch},
         true <- is_binary(entry.x25519_public) or {:error, :no_keyex},
         {:ok, session} <- establish_session(state.identity, entry, &Session.establish/3) do
      state = %{state | sessions: Map.put(state.sessions, peer_key, session)}
      {:ok, session.session_id, state}
    else
      false -> {{:error, :peer_mismatch}, nil, state}
      {:error, _} = error -> {error, nil, state}
      _ -> {{:error, :relay_refresh_failed}, nil, state}
    end
  end

  defp install_refreshed_relay_session(state, _peer_key, {:error, _} = error),
    do: {error, nil, state}

  defp install_refreshed_relay_session(state, _peer_key, _result),
    do: {{:error, :relay_refresh_failed}, nil, state}

  defp pending_relay_request?(state, peer_key) do
    Enum.any?(state.request_routes, fn {{peer, _id}, route} ->
      peer == peer_key and route.generation == :relay
    end)
  end

  defp allow_inbox_message?(state, from_pk, %{kind: kind, request_id: request_id}, generation)
       when kind in [:response, :error] do
    now = System.monotonic_time(:millisecond)

    case Map.get(state.request_routes, {from_pk, request_id}) do
      %{direction: :outgoing, generation: ^generation, deadline: deadline} when deadline > now ->
        {:ok, %{state | request_routes: Map.delete(state.request_routes, {from_pk, request_id})}}

      %{direction: :outgoing} ->
        :drop

      _ ->
        :drop
    end
  end

  defp allow_inbox_message?(state, _from_pk, _message, _generation), do: {:ok, state}

  defp reply_route(state, peer_key, request_id, route_generation, deadline) do
    now = System.monotonic_time(:millisecond)

    case Map.get(state.request_routes, {peer_key, request_id}) do
      %{direction: :incoming, generation: :relay, deadline: expires} when expires > now ->
        {:relay, state}

      %{direction: :incoming, generation: generation, deadline: expires}
      when generation == route_generation and expires > now ->
        if is_pid(state.direct) and Direct.admitted?(state.direct, generation) do
          {:direct, generation, state}
        else
          {:drop, state}
        end

      %{direction: :incoming} ->
        {:drop, state}

      _ when route_generation == :relay and is_nil(deadline) ->
        {:relay, state}

      _ ->
        {:drop, state}
    end
  end

  defp init_handler(nil), do: {:ok, nil}

  defp init_handler(handler_uri) do
    with {:ok, mod, uri} <- Handler.resolve(handler_uri),
         {:ok, handler_state} <- mod.init(uri),
         {:ok, _capability} <- Handler.validate_served_capability(mod, handler_state) do
      {:ok, {mod, handler_state}}
    else
      {:error, {:unknown_scheme, _scheme}} = error -> error
      {:error, reason} -> {:error, {:handler_init_failed, handler_uri, reason}}
    end
  end

  defp receive_packet(packet, state, ingress) do
    with {:ok, decoded} <- Packet.decode(packet),
         true <- decoded.dst == state.identity.public_key,
         false <- reserved_direct_session?(state, decoded),
         {:ok, state} <- enforce_replay_and_freshness(state, decoded) do
      if losing_crossed_offer?(state, decoded, ingress) do
        state
      else
        state
        |> session_for_packet(decoded)
        |> decrypt_and_dispatch(decoded, ingress)
      end
    else
      _ -> state
    end
  end

  defp reserved_direct_session?(%{direct: nil}, _decoded), do: false

  defp reserved_direct_session?(state, decoded),
    do: Direct.reserved_session?(state.direct, decoded.src, decoded.session_id)

  # A losing crossed offer must not discard the initiator's ephemeral relay
  # session: only that initiator still has the key needed to read its acceptance.
  # Authenticate the competing offer in a temporary context before choosing it.
  defp losing_crossed_offer?(%{direct: manager} = state, %{ek: <<_::256>>} = decoded, :relay)
       when is_pid(manager) do
    with %Session{session_id: current} <- state.sessions[decoded.src],
         true <- current != decoded.session_id,
         session <- Session.accept(state.identity, decoded.src, decoded.ek, decoded.session_id),
         {:ok, plaintext} <- Session.decrypt(session, decoded.nonce, decoded.ciphertext),
         {:ok, %{type: :event, meta: %{"topic" => "arc.direct.v1"}, body: body}} <-
           Frame.decode(plaintext),
         true <- byte_size(body) <= 16_384,
         {:ok, control} <- decode_direct_control(body) do
      Direct.losing_offer?(manager, decoded.src, control)
    else
      _ -> false
    end
  end

  defp losing_crossed_offer?(_state, _decoded, _ingress), do: false

  defp decrypt_and_dispatch(state, decoded, ingress) do
    with %Session{} = session <- Map.get(state.sessions, decoded.src),
         {:ok, plaintext} <- Session.decrypt(session, decoded.nonce, decoded.ciphertext) do
      dispatch_plaintext(plaintext, decoded, state, :relay, ingress, nil)
    else
      _ -> state
    end
  end

  defp dispatch_plaintext(plaintext, decoded, state, route_generation, ingress, deadline) do
    case maybe_direct_control(plaintext, decoded, ingress, state) do
      {:handled, state} ->
        state

      :not_control ->
        dispatch_application_plaintext(plaintext, decoded, state, route_generation, deadline)
    end
  end

  defp dispatch_application_plaintext(
         plaintext,
         decoded,
         %{handler: nil} = state,
         route_generation,
         _deadline
       ) do
    append_inbox_message(plaintext, decoded.src, route_generation, state)
  end

  defp dispatch_application_plaintext(plaintext, decoded, state, route_generation, deadline) do
    handle_service_payload(plaintext, decoded, state, route_generation, deadline)
  end

  defp handle_service_payload(plaintext, decoded_packet, state, route_generation, deadline) do
    from_pk = decoded_packet.src

    case Frame.decode(plaintext) do
      {:ok, %{type: type} = frame}
      when type in [:request, :stream_open, :stream_data, :stream_resize, :stream_close] ->
        dispatch_incoming_handler_frame(
          frame,
          from_pk,
          decoded_packet.session_id,
          state,
          route_generation,
          deadline
        )

      {:ok, _frame} ->
        # Service node may still receive responses/errors to its own outgoing requests.
        append_inbox_message(plaintext, from_pk, route_generation, state)

      {:error, _} ->
        # Backward compatibility with legacy plaintext request format.
        legacy_request_id = Frame.new_request_id()

        legacy_frame = %{
          type: :request,
          request_id: legacy_request_id,
          meta: %{},
          body: plaintext
        }

        dispatch_handler_frame(
          legacy_frame,
          from_pk,
          decoded_packet.session_id,
          false,
          state,
          route_generation,
          deadline
        )
    end
  end

  defp dispatch_incoming_handler_frame(
         %{type: :request} = frame,
         from_pk,
         arc_session_id,
         state,
         route_generation,
         deadline
       ) do
    case track_incoming_route(state, from_pk, frame.request_id, route_generation, deadline) do
      {:ok, state} ->
        dispatch_handler_frame(
          frame,
          from_pk,
          arc_session_id,
          true,
          state,
          route_generation,
          deadline
        )

      {:error, _} ->
        state
    end
  end

  defp dispatch_incoming_handler_frame(
         frame,
         from_pk,
         arc_session_id,
         state,
         route_generation,
         deadline
       ) do
    dispatch_handler_frame(
      frame,
      from_pk,
      arc_session_id,
      true,
      state,
      route_generation,
      deadline
    )
  end

  defp dispatch_handler_frame(
         frame,
         from_pk,
         arc_session_id,
         framed?,
         state,
         route_generation,
         deadline
       ) do
    {mod, handler_state} = state.handler
    meta = frame.meta

    context = %{
      from_pk: from_pk,
      meta: meta,
      request_id: frame.request_id,
      frame_type: frame.type,
      arc_session_id: arc_session_id,
      app_session_id: extract_app_session_id(meta),
      framed?: framed?,
      route_generation: route_generation
    }

    notify_observer(state, serve_event(frame, from_pk, context))

    case capability_request(state, meta, frame.type) do
      {:ok, document} ->
        payload = encode_document_response(frame.request_id, meta["path"], document)
        send_reply(state, from_pk, payload, frame.request_id, route_generation, deadline)

      {:error, :not_found} ->
        payload =
          Frame.encode_error(
            frame.request_id,
            "capability_not_found",
            "capability not found"
          )

        send_reply(state, from_pk, payload, frame.request_id, route_generation, deadline)

      :none ->
        result = invoke_handler(mod, frame.type, frame.body, from_pk, context, handler_state)

        process_handler_result(
          result,
          state,
          from_pk,
          frame.request_id,
          framed?,
          route_generation,
          deadline
        )
    end
  end

  defp invoke_handler(mod, frame_type, message, from_pk, context, handler_state) do
    cond do
      frame_type != :request and function_exported?(mod, :handle_frame, 5) ->
        mod.handle_frame(frame_type, message, from_pk, context, handler_state)

      function_exported?(mod, :handle_message, 4) ->
        mod.handle_message(message, from_pk, context, handler_state)

      true ->
        mod.handle_message(message, from_pk, handler_state)
    end
  end

  defp capability_request(%{handler: nil}, _meta, _frame_type), do: :none

  defp capability_request(state, meta, :request) when is_map(meta) do
    case CapabilityManifest.request(meta) do
      {:summary} ->
        {:ok, CapabilityManifest.summary(state.identity, state.handler)}

      {:detail, capability_id} ->
        CapabilityManifest.detail(state.identity, state.handler, capability_id)

      :none ->
        :none
    end
  end

  defp capability_request(_state, _meta, _frame_type), do: :none

  defp process_handler_result(
         {:reply, response, new_handler_state},
         state,
         from_pk,
         request_id,
         framed?,
         route_generation,
         deadline
       ) do
    {mod, _old_handler_state} = state.handler
    state = %{state | handler: {mod, new_handler_state}}
    payload = encode_handler_response(response, request_id, framed?)
    send_reply(state, from_pk, payload, request_id, route_generation, deadline)
  end

  defp process_handler_result(
         {:noreply, new_handler_state},
         state,
         _from_pk,
         _request_id,
         _framed?,
         _route_generation,
         _deadline
       ) do
    {mod, _old_handler_state} = state.handler
    %{state | handler: {mod, new_handler_state}}
  end

  defp process_handler_result(
         {:emit, events, new_handler_state},
         state,
         _from_pk,
         _request_id,
         _framed?,
         _route_generation,
         _deadline
       )
       when is_list(events) do
    {mod, _old_handler_state} = state.handler

    %{state | handler: {mod, new_handler_state}}
    |> emit_handler_events(events)
  end

  defp emit_handler_events(state, events) when is_list(events) do
    Enum.reduce(events, state, &emit_handler_event/2)
  end

  defp emit_handler_event(%{to_pk: to_pk, payload: payload}, state)
       when is_binary(to_pk) and is_binary(payload) do
    case Frame.decode(payload) do
      {:ok, %{request_id: request_id}} -> send_tracked_reply(state, to_pk, payload, request_id)
      _ -> send_reply(state, to_pk, payload)
    end
  end

  defp emit_handler_event(
         %{to_pk: to_pk, frame_type: frame_type, request_id: request_id} = event,
         state
       )
       when is_binary(to_pk) and is_binary(request_id) and is_atom(frame_type) do
    meta = Map.get(event, :meta, %{})
    body = Map.get(event, :body, "")
    payload = Frame.encode_frame(frame_type, request_id, meta, ensure_binary(body))
    send_tracked_reply(state, to_pk, payload, request_id)
  end

  defp emit_handler_event(_event, state), do: state

  # Exec and other asynchronous handlers emit their framed reply later, after
  # the original direct frame has returned from this process. Recover its saved
  # route here so a delayed reply cannot silently move onto the relay.
  defp send_tracked_reply(state, to_pk, payload, request_id) do
    case Map.get(state.request_routes, {to_pk, request_id}) do
      %{direction: :incoming, generation: generation, deadline: deadline} ->
        send_reply(state, to_pk, payload, request_id, generation, deadline)

      nil when is_map_key(state.expired_direct_reply_routes, {to_pk, request_id}) ->
        state

      _ ->
        send_reply(state, to_pk, payload)
    end
  end

  defp encode_handler_response(response, request_id, true) do
    response = ensure_binary(response)

    if error_response?(response) do
      message =
        response
        |> String.replace_prefix("error:", "")
        |> String.trim()

      Frame.encode_error(request_id, "handler_error", blank_to_default(message, "handler error"))
    else
      Frame.encode_response(request_id, %{"status" => 200}, response)
    end
  end

  defp encode_handler_response(response, _request_id, false), do: ensure_binary(response)

  defp encode_document_response(request_id, path, document) do
    body =
      document
      |> :json.encode()
      |> IO.iodata_to_binary()

    Frame.encode_response(
      request_id,
      %{"status" => 200, "content_type" => "application/json", "path" => path},
      body
    )
  end

  defp append_inbox_message(plaintext, from_pk, route_generation, state) do
    received_at = System.monotonic_time(:millisecond)
    from_name = Identity.name(from_pk)

    msg =
      case Frame.decode(plaintext) do
        {:ok, frame} ->
          base = %{
            from: from_name,
            from_key: from_pk,
            text: normalize_frame_text(frame),
            kind: frame.type,
            request_id: frame.request_id,
            request_id_hex: Base.encode16(frame.request_id, case: :lower),
            meta: frame.meta,
            app_session_id: extract_app_session_id(frame.meta),
            received_at: received_at,
            route_generation: route_generation
          }

          if frame.type in [:error, :stream_error] do
            Map.merge(base, %{
              error_code: frame.meta["code"],
              error_message: frame.meta["message"]
            })
          else
            base
          end

        {:error, _} ->
          %{
            from: from_name,
            from_key: from_pk,
            text: plaintext,
            kind: :raw,
            received_at: received_at,
            route_generation: route_generation
          }
      end

    case allow_inbox_message?(state, from_pk, msg, route_generation) do
      {:ok, state} -> %{state | inbox: [msg | state.inbox]}
      :drop -> state
    end
  end

  defp normalize_frame_text(%{type: :error, body: "", meta: meta}) do
    "error: " <> to_string(meta["message"] || "unknown")
  end

  defp normalize_frame_text(%{type: :stream_error, body: "", meta: meta}) do
    "error: " <> to_string(meta["message"] || "unknown")
  end

  defp normalize_frame_text(%{body: body}), do: body

  defp send_reply(state, to_pk, message), do: send_reply(state, to_pk, message, nil, :relay, nil)

  defp send_reply(state, to_pk, message, request_id, route_generation, deadline) do
    case reply_route(state, to_pk, request_id, route_generation, deadline) do
      {:direct, generation, state} ->
        case Direct.send_packet(state.direct, generation, message) do
          :ok ->
            %{state | request_routes: Map.delete(state.request_routes, {to_pk, request_id})}

          {:error, reason} ->
            Logger.warning("direct reply delivery failed: #{inspect(reason)}")
            state
        end

      {:relay, state} ->
        state
        |> send_relay_reply(to_pk, message)
        |> clear_incoming_route(to_pk, request_id)

      {:drop, state} ->
        state
    end
  end

  defp clear_incoming_route(state, peer_key, request_id) do
    case Map.get(state.request_routes, {peer_key, request_id}) do
      %{direction: :incoming} ->
        %{state | request_routes: Map.delete(state.request_routes, {peer_key, request_id})}

      _ ->
        state
    end
  end

  defp send_relay_reply(state, to_pk, message) do
    state = ensure_session(state, to_pk)

    case Map.get(state.sessions, to_pk) do
      %Session{} = session ->
        {nonce, ciphertext, seq, session} = Session.encrypt(session, message)
        state = %{state | sessions: Map.put(state.sessions, to_pk, session)}

        packet =
          Packet.encode(state.identity, to_pk, session.session_id, seq, nonce, ciphertext,
            ek: session.ek_pub
          )

        case deliver(state, to_pk, packet) do
          :ok -> :ok
          {:error, reason} -> Logger.warning("relay reply delivery failed: #{inspect(reason)}")
        end

        state

      nil ->
        state
    end
  end

  # A guard entry protects against replay of packets in one session. A
  # packet older than the skew window is rejected as stale before the guard
  # is consulted, so an entry whose newest packet is older than twice the
  # window can never be hit again and is dropped.
  defp sweep_replay_guard(state, now_ms) do
    horizon = now_ms - 2 * state.allowed_clock_skew_ms

    guard =
      state.replay_guard
      |> Enum.reject(fn {_key, %{max_ts: max_ts}} -> max_ts < horizon end)
      |> Map.new()

    %{state | replay_guard: guard}
  end

  defp schedule_replay_sweep(state) do
    Process.send_after(self(), :sweep_replay_guard, state.allowed_clock_skew_ms)
  end

  defp enforce_replay_and_freshness(state, decoded) do
    now_ms = System.system_time(:millisecond)
    skew = state.allowed_clock_skew_ms

    if abs(now_ms - decoded.ts) > skew do
      {:error, :stale_packet}
    else
      key = {decoded.src, decoded.session_id}

      case Map.get(state.replay_guard, key) do
        nil ->
          guard = %{max_seq: decoded.seq, max_ts: decoded.ts}
          {:ok, %{state | replay_guard: Map.put(state.replay_guard, key, guard)}}

        %{max_seq: max_seq, max_ts: max_ts} = guard when decoded.seq > max_seq ->
          guard = %{guard | max_seq: decoded.seq, max_ts: max(max_ts, decoded.ts)}
          {:ok, %{state | replay_guard: Map.put(state.replay_guard, key, guard)}}

        _ ->
          {:error, :replay}
      end
    end
  end

  # A v2 packet carries the initiator's ephemeral key, so the receiver can
  # always derive the session key. A packet whose session id matches the
  # session already held for that peer reuses it. Any other v2 packet
  # starts a fresh accepted session, which replaces the one held.
  defp session_for_packet(state, %{ek: <<_::binary-size(32)>> = ek} = decoded) do
    case Map.get(state.sessions, decoded.src) do
      %Session{session_id: sid} when sid == decoded.session_id ->
        state

      _ ->
        session = Session.accept(state.identity, decoded.src, ek, decoded.session_id)
        %{state | sessions: Map.put(state.sessions, decoded.src, session)}
    end
  end

  # No ephemeral key: a v1 packet from a peer on the previous release.
  defp session_for_packet(state, decoded) do
    Logger.warning(
      "session v1 packet from #{Identity.name(decoded.src)}; v1 is deprecated and will be removed"
    )

    case Map.get(state.sessions, decoded.src) do
      %Session{version: 1} -> state
      _ -> ensure_session(state, decoded.src, &Session.establish_v1/3)
    end
  end

  defp ensure_session(state, peer_pk, establish \\ &Session.establish/3) do
    if Map.has_key?(state.sessions, peer_pk) do
      state
    else
      peer_name = Identity.name(peer_pk)

      case resolve_entry(state, peer_name) do
        {:ok, [entry]} when entry.x25519_public != nil ->
          case establish_session(state.identity, entry, establish) do
            {:ok, session} ->
              %{state | sessions: Map.put(state.sessions, entry.public_key, session)}

            {:error, _} ->
              state
          end

        _ ->
          state
      end
    end
  end

  defp establish_session(identity, entry, establish) do
    {:ok, establish.(identity, entry.public_key, entry.x25519_public)}
  rescue
    _ -> {:error, :invalid_peer_key}
  end

  defp resolve_peer_key(state, peer_query) do
    match =
      Enum.find(state.sessions, fn {_pk, session} ->
        name = Identity.name(session.peer_public_key)
        short = Identity.short_name(session.peer_public_key)
        pk_hex = Base.encode16(session.peer_public_key, case: :lower)

        name == peer_query or short == peer_query or
          String.starts_with?(pk_hex, String.downcase(peer_query))
      end)

    case match do
      {pk, _session} -> {:ok, pk}
      nil -> {:error, :not_found}
    end
  end

  defp build_outgoing_payload(message, opts) do
    if Keyword.get(opts, :raw, false) do
      message
    else
      request_id = Keyword.get(opts, :request_id, Frame.new_request_id())
      meta = Keyword.get(opts, :meta, %{})
      meta = if is_map(meta), do: meta, else: %{}

      meta =
        case Keyword.get(opts, :app_session_id) || Keyword.get(opts, :sandbox_id) do
          sid when is_binary(sid) and sid != "" -> Map.put_new(meta, "app_session_id", sid)
          _ -> meta
        end

      meta = Map.put_new(meta, "method", "RAW")
      Frame.encode_request(request_id, meta, message)
    end
  end

  defp ensure_request_id(opts) do
    if Keyword.get(opts, :raw, false) or
         match?(<<_::binary-size(16)>>, Keyword.get(opts, :request_id)) do
      opts
    else
      Keyword.put(opts, :request_id, Frame.new_request_id())
    end
  end

  defp extract_app_session_id(meta) when is_map(meta) do
    cond do
      is_binary(meta["app_session_id"]) and meta["app_session_id"] != "" ->
        meta["app_session_id"]

      is_binary(meta["sandbox_id"]) and meta["sandbox_id"] != "" ->
        meta["sandbox_id"]

      is_binary(meta["app.session_id"]) and meta["app.session_id"] != "" ->
        meta["app.session_id"]

      is_map(meta["app"]) and is_binary(meta["app"]["session_id"]) and
          meta["app"]["session_id"] != "" ->
        meta["app"]["session_id"]

      true ->
        nil
    end
  end

  defp ensure_binary(value) when is_binary(value), do: value
  defp ensure_binary(value) when is_list(value), do: IO.iodata_to_binary(value)
  defp ensure_binary(value), do: inspect(value)

  defp take_first_matching([], _matcher, _prefix), do: :error

  defp take_first_matching([message | rest], matcher, prefix) do
    if matcher.(message) do
      {:ok, message, Enum.reverse(prefix) ++ rest}
    else
      take_first_matching(rest, matcher, [message | prefix])
    end
  end

  defp error_response?(value) when is_binary(value) do
    value
    |> String.downcase()
    |> String.starts_with?("error:")
  end

  defp blank_to_default("", fallback), do: fallback
  defp blank_to_default(value, _fallback), do: value

  defp deliver(%{relay_discovery: true, identity: identity}, to_pk, packet) do
    relay_call(:deliver_via_relay, [identity.public_key, to_pk, packet])
  end

  defp deliver(%{identity: identity}, to_pk, packet) do
    from_pk = identity.public_key

    case Registry.lookup(Arc.Data.AgentRegistry, to_pk) do
      [{pid, _}] ->
        send(pid, {:arc_packet, packet})
        :ok

      [] ->
        if Code.ensure_loaded?(Arc.Net) and function_exported?(Arc.Net, :deliver, 3) do
          # Arc.Net is an optional runtime peer, not a compile-time dep.
          # credo:disable-for-next-line Credo.Check.Refactor.Apply
          apply(Arc.Net, :deliver, [from_pk, to_pk, packet])
        else
          Mailbox.deliver(to_pk, packet)
        end
    end
  end

  defp resolve_entry(%{relay_discovery: true, identity: identity}, query),
    do: relay_call(:resolve_via_relay, [identity.public_key, query])

  defp resolve_entry(_state, query) do
    case Control.resolve(query) do
      {:ok, [entry | _]} -> {:ok, [entry]}
      result -> result
    end
  end

  defp announce_relay(state) do
    capabilities =
      if state.handler do
        CapabilityManifest.summary(state.identity, state.handler)["capabilities"]
      else
        []
      end

    with {:ok, announcement_opts} <- relay_announcement_opts(state) do
      record = RelayAnnouncement.create(state.identity, capabilities, announcement_opts)
      relay_call(:announce, [state.identity.public_key, record])
    end
  rescue
    ArgumentError -> {:error, :invalid_announcement}
  end

  defp relay_announcement_opts(%{relay_federation: :local}), do: {:ok, []}

  defp relay_announcement_opts(%{relay_federation: federation, identity: identity})
       when federation in [:direct, :network] do
    with {:ok, relay_public_key} <- relay_call(:relay_public_key, [identity.public_key]),
         true <- is_binary(relay_public_key) and byte_size(relay_public_key) == 32 do
      {:ok, [federation: federation, relay_public_key: relay_public_key]}
    else
      false -> {:error, :invalid_relay_public_key}
      {:error, _reason} = error -> error
      _ -> {:error, :invalid_relay_public_key}
    end
  end

  defp relay_announcement_opts(_), do: {:error, :invalid_federation}

  defp schedule_announcement(state) do
    if state.announcement_timer, do: Process.cancel_timer(state.announcement_timer)
    generation = make_ref()
    timer = Process.send_after(self(), {:refresh_relay_announcement, generation}, 60_000)
    %{state | announcement_timer: timer, announcement_generation: generation}
  end

  defp relay_call(function, args) do
    if Code.ensure_loaded?(Arc.Net) and function_exported?(Arc.Net, function, length(args)) do
      # Arc.Net depends on arc_data; keep the reverse runtime dependency optional.
      # credo:disable-for-next-line Credo.Check.Refactor.Apply
      apply(Arc.Net, function, args)
    else
      {:error, :relay_runtime_unavailable}
    end
  catch
    :exit, _ -> {:error, :relay_not_connected}
  end

  defp serve_event(frame, from_pk, context) do
    base = %{
      type: frame.type,
      from: Identity.short_name(from_pk),
      peer_key: short_public_key(from_pk),
      app_session_id: context.app_session_id,
      method: context.meta["method"],
      path: context.meta["path"],
      body: frame.body
    }

    case frame.type do
      :stream_data ->
        Map.put(base, :bytes, byte_size(frame.body))

      :stream_resize ->
        base
        |> Map.put(:cols, context.meta["cols"])
        |> Map.put(:rows, context.meta["rows"])

      _ ->
        base
    end
  end

  defp notify_observer(%{observer: pid}, event) when is_pid(pid) and is_map(event) do
    send(pid, {:arc_serve_event, event})
    :ok
  end

  defp notify_observer(_state, _event), do: :ok

  defp short_public_key(binary) when is_binary(binary) and byte_size(binary) > 0 do
    pk_hex = Base.encode16(binary, case: :lower)
    binary_part(pk_hex, 0, 4) <> "…" <> binary_part(pk_hex, byte_size(pk_hex), -4)
  end

  defp short_public_key(_), do: "unknown"
end
