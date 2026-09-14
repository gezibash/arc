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
  alias Arc.Data.Frame
  alias Arc.Data.Handler
  alias Arc.Data.Mailbox
  alias Arc.Data.Packet
  alias Arc.Data.Session
  alias Arc.Identity

  @default_allowed_clock_skew_ms 120_000

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
    GenServer.start_link(__MODULE__, {identity, handler_uri, observer}, opts)
  end

  def publish(agent) do
    GenServer.call(agent, :publish)
  end

  def connect(agent, peer_query) when is_binary(peer_query) do
    GenServer.call(agent, {:connect, peer_query})
  end

  def send_message(agent, peer_query, message) when is_binary(message) do
    send_message(agent, peer_query, message, [])
  end

  def send_message(agent, peer_query, message, opts)
      when is_binary(message) and is_list(opts) do
    GenServer.call(agent, {:send, peer_query, message, opts})
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

  # --- GenServer Callbacks ---

  @impl GenServer
  def init({%Identity{} = identity, handler_uri, observer}) do
    with {:ok, _} <- Registry.register(Arc.Data.AgentRegistry, identity.public_key, []),
         {:ok, handler} <- init_handler(handler_uri) do
      state = %{
        identity: identity,
        sessions: %{},
        inbox: [],
        handler: handler,
        observer: observer,
        replay_guard: %{},
        allowed_clock_skew_ms:
          Application.get_env(:arc_data, :allowed_clock_skew_ms, @default_allowed_clock_skew_ms)
      }

      schedule_replay_sweep(state)
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

  def handle_call({:connect, peer_query}, _from, state) do
    case Control.resolve(peer_query) do
      {:ok, [entry | _]} ->
        if entry.x25519_public do
          session = Session.establish(state.identity, entry.public_key, entry.x25519_public)
          sessions = Map.put(state.sessions, entry.public_key, session)
          {:reply, {:ok, entry}, %{state | sessions: sessions}}
        else
          {:reply, {:error, :no_keyex}, state}
        end

      {:ok, []} ->
        {:reply, {:error, :not_found}, state}

      error ->
        {:reply, error, state}
    end
  end

  def handle_call({:send, peer_query, message, opts}, _from, state) do
    with {:ok, peer_pk} <- resolve_peer_key(state, peer_query),
         %Session{} = session <- Map.get(state.sessions, peer_pk) do
      payload = build_outgoing_payload(message, opts)
      {nonce, ciphertext, seq, session} = Session.encrypt(session, payload)
      state = %{state | sessions: Map.put(state.sessions, peer_pk, session)}

      packet =
        Packet.encode(state.identity, peer_pk, session.session_id, seq, nonce, ciphertext,
          ek: session.ek_pub
        )

      deliver(state.identity.public_key, peer_pk, packet)
      {:reply, :ok, state}
    else
      nil -> {:reply, {:error, :no_session}, state}
      error -> {:reply, error, state}
    end
  end

  def handle_call(:read_inbox, _from, state) do
    {:reply, Enum.reverse(state.inbox), %{state | inbox: []}}
  end

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
      serving: handler_scheme
    }

    {:reply, info, state}
  end

  @impl GenServer
  def handle_cast(:poll_mailbox, state) do
    packets = Mailbox.read(state.identity.public_key)
    state = Enum.reduce(packets, state, &receive_packet/2)
    {:noreply, state}
  end

  @impl GenServer
  def handle_info({:arc_packet, packet}, state) do
    {:noreply, receive_packet(packet, state)}
  end

  def handle_info(:sweep_replay_guard, state) do
    state = sweep_replay_guard(state, System.system_time(:millisecond))
    schedule_replay_sweep(state)
    {:noreply, state}
  end

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

  # --- Private ---

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

  defp receive_packet(packet, state) do
    with {:ok, decoded} <- Packet.decode(packet),
         true <- decoded.dst == state.identity.public_key,
         {:ok, state} <- enforce_replay_and_freshness(state, decoded) do
      state
      |> session_for_packet(decoded)
      |> decrypt_and_dispatch(decoded)
    else
      _ -> state
    end
  end

  defp decrypt_and_dispatch(state, decoded) do
    with %Session{} = session <- Map.get(state.sessions, decoded.src),
         {:ok, plaintext} <- Session.decrypt(session, decoded.nonce, decoded.ciphertext) do
      dispatch_plaintext(plaintext, decoded, state)
    else
      _ -> state
    end
  end

  defp dispatch_plaintext(plaintext, decoded, %{handler: nil} = state) do
    append_inbox_message(plaintext, decoded.src, state)
  end

  defp dispatch_plaintext(plaintext, decoded, state) do
    handle_service_payload(plaintext, decoded, state)
  end

  defp handle_service_payload(plaintext, decoded_packet, state) do
    from_pk = decoded_packet.src

    case Frame.decode(plaintext) do
      {:ok, %{type: type} = frame}
      when type in [:request, :stream_open, :stream_data, :stream_resize, :stream_close] ->
        dispatch_handler_frame(frame, from_pk, decoded_packet.session_id, true, state)

      {:ok, _frame} ->
        # Service node may still receive responses/errors to its own outgoing requests.
        append_inbox_message(plaintext, from_pk, state)

      {:error, _} ->
        # Backward compatibility with legacy plaintext request format.
        legacy_request_id = Frame.new_request_id()

        legacy_frame = %{
          type: :request,
          request_id: legacy_request_id,
          meta: %{},
          body: plaintext
        }

        dispatch_handler_frame(legacy_frame, from_pk, decoded_packet.session_id, false, state)
    end
  end

  defp dispatch_handler_frame(frame, from_pk, arc_session_id, framed?, state) do
    {mod, handler_state} = state.handler
    meta = frame.meta

    context = %{
      from_pk: from_pk,
      meta: meta,
      request_id: frame.request_id,
      frame_type: frame.type,
      arc_session_id: arc_session_id,
      app_session_id: extract_app_session_id(meta),
      framed?: framed?
    }

    notify_observer(state, serve_event(frame, from_pk, context))

    case capability_request(state, meta, frame.type) do
      {:ok, document} ->
        payload = encode_document_response(frame.request_id, meta["path"], document)
        send_reply(state, from_pk, payload)

      {:error, :not_found} ->
        payload =
          Frame.encode_error(
            frame.request_id,
            "capability_not_found",
            "capability not found"
          )

        send_reply(state, from_pk, payload)

      :none ->
        result = invoke_handler(mod, frame.type, frame.body, from_pk, context, handler_state)
        process_handler_result(result, state, from_pk, frame.request_id, framed?)
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
         framed?
       ) do
    {mod, _old_handler_state} = state.handler
    state = %{state | handler: {mod, new_handler_state}}
    payload = encode_handler_response(response, request_id, framed?)
    send_reply(state, from_pk, payload)
  end

  defp process_handler_result(
         {:noreply, new_handler_state},
         state,
         _from_pk,
         _request_id,
         _framed?
       ) do
    {mod, _old_handler_state} = state.handler
    %{state | handler: {mod, new_handler_state}}
  end

  defp process_handler_result(
         {:emit, events, new_handler_state},
         state,
         _from_pk,
         _request_id,
         _framed?
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
    send_reply(state, to_pk, payload)
  end

  defp emit_handler_event(
         %{to_pk: to_pk, frame_type: frame_type, request_id: request_id} = event,
         state
       )
       when is_binary(to_pk) and is_binary(request_id) and is_atom(frame_type) do
    meta = Map.get(event, :meta, %{})
    body = Map.get(event, :body, "")
    payload = Frame.encode_frame(frame_type, request_id, meta, ensure_binary(body))
    send_reply(state, to_pk, payload)
  end

  defp emit_handler_event(_event, state), do: state

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

  defp append_inbox_message(plaintext, from_pk, state) do
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
            received_at: received_at
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
            received_at: received_at
          }
      end

    %{state | inbox: [msg | state.inbox]}
  end

  defp normalize_frame_text(%{type: :error, body: "", meta: meta}) do
    "error: " <> to_string(meta["message"] || "unknown")
  end

  defp normalize_frame_text(%{type: :stream_error, body: "", meta: meta}) do
    "error: " <> to_string(meta["message"] || "unknown")
  end

  defp normalize_frame_text(%{body: body}), do: body

  defp send_reply(state, to_pk, message) do
    state = ensure_session(state, to_pk)

    case Map.get(state.sessions, to_pk) do
      %Session{} = session ->
        {nonce, ciphertext, seq, session} = Session.encrypt(session, message)
        state = %{state | sessions: Map.put(state.sessions, to_pk, session)}

        packet =
          Packet.encode(state.identity, to_pk, session.session_id, seq, nonce, ciphertext,
            ek: session.ek_pub
          )

        deliver(state.identity.public_key, to_pk, packet)
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

      case Control.resolve(peer_name) do
        {:ok, [entry | _]} when entry.x25519_public != nil ->
          session = establish.(state.identity, entry.public_key, entry.x25519_public)
          %{state | sessions: Map.put(state.sessions, entry.public_key, session)}

        _ ->
          state
      end
    end
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

  defp deliver(from_pk, to_pk, packet) do
    case Registry.lookup(Arc.Data.AgentRegistry, to_pk) do
      [{pid, _}] ->
        send(pid, {:arc_packet, packet})

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
