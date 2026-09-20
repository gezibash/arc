defmodule Arc.Data.Direct do
  @moduledoc """
  Owner-scoped direct conversations negotiated exclusively through ARC relays.

  The agent owns ordinary relay sessions and provider dispatch. This process owns
  separate direct crypto contexts, bounded attempts, and monotonic consent leases.
  It never calls its agent synchronously: expiry remains enforceable while relay
  discovery is unavailable. A carrier proves both keys before admitting records.
  """
  use GenServer

  alias Arc.Data.{CapabilityPackage, Frame, Packet, Session}
  alias Arc.Data.Direct.Policy

  @topic "arc.direct.v1"
  @attempt_ms 3_000
  @max_routes 32
  @max_tombstones 512
  @tombstone_ms 240_000
  @cooldown_ms 5_000

  def start_link(opts), do: GenServer.start_link(__MODULE__, opts)

  def route(manager, target, capability \\ "primary") do
    GenServer.call(manager, {:route, target, capability})
  catch
    :exit, _ -> nil
  end

  def allowed?(manager, target, capability \\ "primary") do
    GenServer.call(manager, {:allowed, target, capability})
  catch
    :exit, _ -> false
  end

  def promote(manager, target, package, timeout_ms \\ @attempt_ms) do
    GenServer.call(manager, {:promote, target, package, timeout_ms}, @attempt_ms + 1_000)
  catch
    :exit, _ -> {:error, :direct_unavailable}
  end

  def send_packet(manager, generation, payload) do
    GenServer.call(manager, {:send, generation, payload}, 2_000)
  catch
    :exit, _ -> {:error, :outcome_unknown}
  end

  def admitted?(manager, generation) do
    GenServer.call(manager, {:admitted, generation})
  catch
    :exit, _ -> false
  end

  def reserved_session?(manager, peer, session_id) do
    GenServer.call(manager, {:reserved_session, peer, session_id})
  catch
    :exit, _ -> true
  end

  def refreshing?(manager, ref) do
    GenServer.call(manager, {:refreshing, ref})
  catch
    :exit, _ -> false
  end

  def losing_offer?(manager, peer, message) do
    GenServer.call(manager, {:losing_offer, peer, message})
  catch
    :exit, _ -> false
  end

  def revoke(manager, generation), do: GenServer.call(manager, {:revoke, generation})

  def status(manager), do: GenServer.call(manager, :status)

  @impl true
  def init(opts) do
    case Policy.normalize(Keyword.fetch!(opts, :policy)) do
      {:ok, rules} ->
        owner = Keyword.fetch!(opts, :owner)

        {:ok,
         %{
           identity: Keyword.fetch!(opts, :identity),
           owner: owner,
           owner_ref: Process.monitor(owner),
           policy: rules,
           routes: %{},
           effects: %{},
           tombstones: %{},
           cooldowns: %{},
           relay_up: true
         }}

      error ->
        {:stop, error}
    end
  end

  @impl true
  def handle_call({:allowed, target, capability}, _from, state) do
    {:reply, not is_nil(Policy.find(state.policy, target.key, scope_query(target, capability))),
     state}
  end

  def handle_call({:route, target, capability}, _from, state) do
    match =
      Enum.find_value(state.routes, fn {_id, route} ->
        if route.role == :caller and route.peer == target.key and
             scope_matches?(route.scope, scope_query(target, capability)) and usable?(route),
           do: public_route(route)
      end)

    {:reply, match, state}
  end

  def handle_call({:admitted, id}, _from, state),
    do: {:reply, usable?(state.routes[id]), state}

  def handle_call({:losing_offer, peer, message}, _from, state) do
    scope = message["scope"]

    loser? =
      message["type"] == "offer" and valid_id?(message["id"]) and
        valid_scope?(scope, peer, state.identity.public_key) and
        Enum.any?(state.routes, fn {_, route} ->
          route.role == :caller and route.phase == :offered and route.peer == peer and
            scope_matches?(route.scope, Map.take(scope, ~w(capability scheme path))) and
            {hex(state.identity.public_key), route.id} < {hex(peer), message["id"]}
        end)

    {:reply, loser?, state}
  end

  def handle_call({:refreshing, ref}, _from, state) do
    current =
      case state.effects[ref] do
        {id, :refresh} ->
          match?(%{renewal: %{ref: ^ref}}, state.routes[id]) and usable?(state.routes[id])

        _ ->
          false
      end

    {:reply, current, state}
  end

  def handle_call({:reserved_session, peer, sid}, _from, state) do
    reserved =
      Enum.any?(state.routes, fn {_, route} ->
        route.peer == peer and route.session.session_id == sid
      end) or
        Enum.any?(state.tombstones, fn {_, tombstone} ->
          tombstone.peer == peer and tombstone.session_id == sid and tombstone.deadline > now()
        end)

    {:reply, reserved, state}
  end

  def handle_call(:status, _from, state) do
    status =
      Enum.map(state.routes, fn {id, route} ->
        %{
          generation: id,
          phase: route.phase,
          role: route.role,
          peer: Base.encode16(route.peer, case: :lower),
          remaining_ms: max(0, route.deadline - now()),
          relay_available: state.relay_up
        }
      end)

    {:reply, status, state}
  end

  def handle_call({:promote, target, package, timeout}, from, state) do
    state = sweep(state)
    query = scope_query(target, get_in(package, ["capability", "id"]))
    rule = Policy.find(state.policy, target.key, query)

    with true <- is_integer(timeout) and timeout > 0,
         true <- not is_nil(rule),
         true <- room?(state),
         true <- not busy?(state, target.key, query),
         true <- Map.get(state.cooldowns, {target.key, query}, now()) <= now(),
         {:ok, verified} <- CapabilityPackage.verify(package),
         true <- get_in(verified, ["provider", "public_key"]) == target.provider,
         true <- get_in(verified, ["capability", "scheme"]) == target.scheme,
         true <- get_in(verified, ["capability", "invocation", "mode"]) == "request_reply",
         {:ok, credentials} <- carrier(:credentials, []),
         {:ok, session} <- Session.establish(state.identity, target.key) do
      id = random_id()

      scope =
        Map.merge(query, %{
          "caller" => hex(state.identity.public_key),
          "provider" => target.provider,
          "package_hash" => verified["package_hash"],
          "method" => get_in(verified, ["capability", "invocation", "method"]),
          "session" => hex(session.session_id),
          "ek" => hex(session.ek_pub),
          "version" => 1
        })

      route = new_route(id, target.key, scope, rule, :caller, credentials, session)
      route = %{route | from: from, package: verified}
      Process.send_after(self(), {:attempt_timeout, id}, min(timeout, @attempt_ms))
      state = put_route(state, route)

      {:noreply,
       control(
         state,
         route,
         %{
           "type" => "offer",
           "scope" => scope,
           "fingerprint" => hex(credentials.fingerprint),
           "lease_ms" => rule.lease_ms,
           "hole_punch" => rule.hole_punch
         },
         :offer
       )}
    else
      _ -> {:reply, {:error, :direct_not_available}, state}
    end
  rescue
    _ -> {:reply, {:error, :direct_not_available}, state}
  end

  def handle_call({:send, id, payload}, _from, state) do
    case state.routes[id] do
      route when is_map(route) ->
        with true <- usable?(route),
             {:ok, frame} <- Frame.decode(payload),
             true <- allowed_frame?(route, frame, :outgoing),
             {:ok, state} <- write_packet(state, route, payload) do
          {:reply, :ok, state}
        else
          false -> {:reply, {:error, :direct_unavailable}, state}
          {:error, :outcome_unknown, state} -> {:reply, {:error, :outcome_unknown}, state}
          _ -> {:reply, {:error, :invalid_direct_frame}, state}
        end

      _ ->
        {:reply, {:error, :direct_unavailable}, state}
    end
  end

  def handle_call({:revoke, id}, _from, state) do
    case state.routes[id] do
      nil ->
        {:reply, :ok, state}

      route ->
        state = control(state, route, %{"type" => "withdraw"}, :best_effort)
        payload = Frame.encode_event(@topic, json(%{"type" => "withdraw", "id" => id}))

        state =
          case write_packet(state, route, payload) do
            {:ok, next} -> next
            {:error, _, next} -> next
          end

        # A local withdrawal also removes the saved rule for this process.
        policy = Enum.reject(state.policy, &(&1 == route.rule))
        {:reply, :ok, retire(%{state | policy: policy}, id, :withdrawn)}
    end
  end

  @impl true
  def handle_info(
        {:arc_direct_control, peer, sid, %{"type" => "offer", "id" => id} = message},
        state
      ) do
    {:noreply, receive_offer(sweep(state), peer, sid, id, message)}
  end

  def handle_info({:arc_direct_control, peer, sid, %{"id" => id} = message}, state) do
    case state.routes[id] do
      %{peer: ^peer} = route ->
        if sid == route.relay_sid or message["type"] in ["renew", "renewed"] do
          {:noreply, receive_control(state, route, sid, message)}
        else
          {:noreply, state}
        end

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:arc_direct_validated, ref, result}, state) do
    {effect, state} = pop_effect(state, ref)

    case effect do
      {id, :validate_offer} ->
        case {state.routes[id], result} do
          {%{} = route, {:ok, package}} ->
            {:noreply, accept_offer(state, %{route | package: package})}

          _ ->
            {:noreply, retire(state, id, :capability_changed)}
        end

      {id, {:validate_renewal, message, sid}} ->
        case {state.routes[id], result} do
          {%{} = route, {:ok, _package}} -> {:noreply, accept_renewal(state, route, message, sid)}
          _ -> {:noreply, retire(state, id, :capability_changed)}
        end

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:arc_direct_control_result, ref, result, sid}, state) do
    {effect, state} = pop_effect(state, ref)

    case effect do
      {id, action} ->
        case state.routes[id] do
          nil -> {:noreply, state}
          route -> {:noreply, control_result(state, route, action, result, sid)}
        end

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:arc_direct_refreshed, ref, result, sid}, state) do
    {effect, state} = pop_effect(state, ref)

    case {effect, result} do
      {{id, :refresh}, :ok} ->
        case state.routes[id] do
          route when is_map(route) ->
            if usable?(route) do
              renewal = %{nonce: random_id(), started: now(), sid: sid, ref: ref}
              route = %{route | renewal: renewal}
              state = put_route(state, route)

              {:noreply,
               control(
                 state,
                 route,
                 %{
                   "type" => "renew",
                   "nonce" => renewal.nonce,
                   "lease_ms" => route.lease_ms,
                   "package_hash" => route.scope["package_hash"]
                 },
                 :renew
               )}
            else
              {:noreply, retire(state, id, :expired)}
            end

          _ ->
            {:noreply, state}
        end

      {{id, :refresh}, _} ->
        {:noreply, retry_renewal(state, id)}

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:arc_direct_prepared, ref, result}, state) do
    {effect, state} = pop_effect(state, ref)

    case {effect, result} do
      {{id, :prepare}, :ok} -> {:noreply, arm_route(state, id, ref)}
      {{id, :prepare}, _} -> {:noreply, retire(state, id, :busy)}
      _ -> {:noreply, state}
    end
  end

  def handle_info({:arc_direct_connected, {id, direction}, pid}, state) do
    case state.routes[id] do
      route when is_map(route) ->
        if valid_connection?(route.connections[direction], pid) and route.deadline > now() do
          route = %{
            route
            | connected: MapSet.put(route.connected, direction),
              connections: Map.put(route.connections, direction, pid)
          }

          state = put_route(state, route)

          cond do
            route.role == :caller and is_nil(route.selected) ->
              {:noreply, prepare(state, %{route | selected: direction})}

            route.role == :provider and route.selected == direction and route.phase == :probing ->
              {:noreply, prepare(state, route)}

            true ->
              {:noreply, state}
          end
        else
          carrier(:close, [pid])
          {:noreply, state}
        end

      _ ->
        carrier(:close, [pid])
        {:noreply, state}
    end
  end

  def handle_info({:dial_result, id, direction, worker, result}, state) do
    case state.routes[id] do
      %{connections: connections} = route ->
        case {connections[direction], result} do
          {{:dialing, ^worker}, {:ok, pid}} ->
            {:noreply,
             put_route(state, %{route | connections: Map.put(connections, direction, pid)})}

          {{:dialing, ^worker}, _} ->
            {:noreply,
             put_route(state, %{route | connections: Map.delete(connections, direction)})}

          {pid, {:ok, pid}} ->
            {:noreply, state}

          {_, {:ok, pid}} ->
            carrier(:close, [pid])
            {:noreply, state}

          _ ->
            {:noreply, state}
        end

      _ ->
        if match?({:ok, _}, result), do: carrier(:close, [elem(result, 1)])
        {:noreply, state}
    end
  end

  def handle_info({:punch_endpoint, id, worker, result}, state) do
    case state.routes[id] do
      %{phase: :probing, hole_punch: true, punch_worker: ^worker} = route ->
        {:noreply, punch_endpoint(state, %{route | punch_worker: nil}, result)}

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:arc_direct_closed, {id, direction}, pid, _reason}, state) do
    case state.routes[id] do
      route when is_map(route) ->
        cond do
          route.connections[direction] != pid ->
            {:noreply, state}

          route.selected == direction ->
            {:noreply, retire(state, id, :direct_failed)}

          true ->
            route = %{
              route
              | connections: Map.delete(route.connections, direction),
                connected: MapSet.delete(route.connected, direction)
            }

            {:noreply, put_route(state, route)}
        end

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:arc_direct_packet, {id, direction}, pid, packet}, state) do
    case state.routes[id] do
      route when is_map(route) ->
        if route.selected == direction and route.connections[direction] == pid and usable?(route) do
          {:noreply, receive_packet(state, route, packet)}
        else
          {:noreply, state}
        end

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:attempt_timeout, id}, state) do
    case state.routes[id] do
      %{phase: :active} -> {:noreply, state}
      _ -> {:noreply, retire(state, id, :direct_timeout)}
    end
  end

  def handle_info({:lease_expired, id, deadline}, state) do
    case state.routes[id] do
      %{deadline: ^deadline} -> {:noreply, retire(state, id, :expired)}
      _ -> {:noreply, state}
    end
  end

  def handle_info({:renew, id, deadline}, state) do
    case state.routes[id] do
      %{role: :caller, deadline: ^deadline, renewal: nil} = route ->
        if usable?(route) do
          {ref, state} = effect(state, id, :refresh)
          send(state.owner, {:arc_direct_refresh, self(), ref, route.peer})
          # Bound a stalled refresh/ack independently from the lease timer.
          Process.send_after(
            self(),
            {:renewal_timeout, id, ref},
            min(11_000, max(1, route.deadline - now()))
          )

          {:noreply, put_route(state, %{route | renewal: %{ref: ref}})}
        else
          {:noreply, retire(state, id, :expired)}
        end

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:renewal_timeout, id, ref}, state) do
    case state.routes[id] do
      %{renewal: %{ref: ^ref}} ->
        # Remove any abandoned refresh effect; its late reply cannot start renewal.
        state = %{state | effects: Map.delete(state.effects, ref)}
        {:noreply, retry_renewal(state, id)}

      _ ->
        {:noreply, state}
    end
  end

  def handle_info({:arc_relay_status, :down}, state) do
    # A new punched path must use the source port of its current relay socket.
    # Losing that socket abandons preparation, never an already admitted lease.
    state =
      Enum.reduce(state.routes, state, fn
        {id, %{hole_punch: true, phase: phase}}, acc when phase != :active ->
          retire(acc, id, :relay_unavailable)

        _, acc ->
          acc
      end)

    {:noreply, %{state | relay_up: false}}
  end

  def handle_info({:arc_relay_status, :up}, state),
    do: {:noreply, %{state | relay_up: true}}

  def handle_info({:DOWN, ref, :process, _pid, _reason}, %{owner_ref: ref} = state),
    do: {:stop, :normal, state}

  def handle_info(_, state), do: {:noreply, state}

  @impl true
  def format_status(status), do: Map.put(status, :state, :redacted)

  @impl true
  def terminate(_, state) do
    Enum.each(state.routes, fn {_, route} -> close_connections(route) end)
  end

  defp receive_offer(state, peer, sid, id, message) do
    scope = message["scope"]

    with true <- valid_id?(id) and not Map.has_key?(state.tombstones, id),
         true <- room?(state),
         true <- is_binary(sid) and byte_size(sid) == 16,
         true <- valid_scope?(scope, peer, state.identity.public_key),
         rule when not is_nil(rule) <- Policy.find(state.policy, peer, scope),
         lease when is_integer(lease) and lease in 1_000..120_000 <- message["lease_ms"],
         {:ok, fingerprint} <- unhex(message["fingerprint"], 32),
         {:ok, ek} <- unhex(scope["ek"], 32),
         {:ok, session_id} <- unhex(scope["session"], 16),
         {:ok, state} <- admit_offer(state, peer, scope, id),
         {:ok, credentials} <- carrier(:credentials, []) do
      session = Session.accept(state.identity, peer, ek, session_id)
      route = new_route(id, peer, scope, rule, :provider, credentials, session)

      route = %{
        route
        | phase: :validating,
          peer_fingerprint: fingerprint,
          relay_sid: sid,
          hole_punch: rule.hole_punch and message["hole_punch"] == true,
          lease_ms: min(rule.lease_ms, lease),
          deadline: now() + min(rule.lease_ms, lease)
      }

      state = put_route(state, route)
      Process.send_after(self(), {:attempt_timeout, id}, @attempt_ms)
      {ref, state} = effect(state, id, :validate_offer)

      send(
        state.owner,
        {:arc_direct_validate, self(), ref, scope["capability"], scope["package_hash"]}
      )

      state
    else
      _ -> state
    end
  rescue
    _ -> state
  end

  defp admit_offer(state, peer, scope, id) do
    case Enum.find_value(state.routes, fn {_, route} ->
           if route.peer == peer and
                scope_matches?(route.scope, Map.take(scope, ~w(capability scheme path))),
              do: route
         end) do
      nil ->
        {:ok, state}

      %{role: :caller, phase: :offered} = route ->
        if {hex(peer), id} < {hex(state.identity.public_key), route.id},
          do: {:ok, retire(state, route.id, :superseded)},
          else: {:error, :busy}

      _ ->
        {:error, :busy}
    end
  end

  defp accept_offer(state, route) do
    if route.deadline <= now() or not package_matches?(route) do
      retire(state, route.id, :invalid_scope)
    else
      route = %{route | binding: context_binding(route), phase: :probing}
      {route, candidate} = listen(route, state)
      state = put_route(state, route)

      control(
        state,
        route,
        %{
          "type" => "accept",
          "lease_ms" => route.lease_ms,
          "fingerprint" => hex(route.credentials.fingerprint),
          "candidate" => candidate,
          "hole_punch" => route.hole_punch
        },
        :accept
      )
      |> observe_punch_endpoint(route)
    end
  end

  defp receive_control(
         state,
         %{role: :caller, phase: :offered} = route,
         _sid,
         %{"type" => "accept", "lease_ms" => lease, "fingerprint" => fingerprint} = message
       ) do
    with true <- is_integer(lease) and lease in 1_000..route.lease_ms,
         true <- valid_punch_accept?(route, message),
         {:ok, fingerprint} <- unhex(fingerprint, 32) do
      route = %{
        route
        | peer_fingerprint: fingerprint,
          lease_ms: lease,
          deadline: min(route.deadline, now() + lease),
          hole_punch: message["hole_punch"] == true,
          phase: :probing
      }

      route = %{route | binding: context_binding(route)}
      {route, candidate} = listen(route, state)
      route = dial(route, state, "provider", message["candidate"])
      state = put_route(state, route)

      control(state, route, %{"type" => "candidates", "candidate" => candidate}, :candidates)
      |> observe_punch_endpoint(route)
    else
      _ -> retire(state, route.id, :invalid_accept)
    end
  end

  defp receive_control(
         state,
         %{phase: :probing, hole_punch: true, peer_punch: nil} = route,
         _sid,
         %{"type" => "punch_candidate", "candidate" => candidate}
       ) do
    case Policy.candidate(route.rule, candidate) do
      {:ok, host, port} ->
        route = %{route | peer_punch: {host, port}}
        put_route(state, start_punch(route, state))

      _ ->
        state
    end
  end

  defp receive_control(
         state,
         %{role: :provider, phase: :probing} = route,
         _sid,
         %{"type" => "candidates"} = message
       ) do
    put_route(state, dial(route, state, "caller", message["candidate"]))
  end

  defp receive_control(state, %{role: :provider, phase: :probing, selected: nil} = route, _sid, %{
         "type" => "nominate",
         "direction" => direction
       })
       when direction in ["provider", "caller", "punch"] do
    if pending_connection?(direction, route.connections[direction]) do
      route = %{route | selected: direction}

      if MapSet.member?(route.connected, direction),
        do: prepare(state, route),
        else: put_route(state, route)
    else
      retire(state, route.id, :invalid_nomination)
    end
  end

  defp receive_control(state, %{role: :caller, phase: :preparing} = route, _sid, %{
         "type" => "ready",
         "direction" => direction
       }) do
    if (direction == route.selected and route.prepared) && route.deadline > now() do
      route = %{route | phase: :active}
      state = put_route(state, route) |> schedule_lease(route)
      release(state, route)
      if route.from, do: GenServer.reply(route.from, {:ok, public_route(route)})
      put_route(state, %{route | from: nil})
    else
      retire(state, route.id, :invalid_readiness)
    end
  end

  defp receive_control(state, route, _sid, %{"type" => "withdraw"}),
    do: retire(state, route.id, :withdrawn)

  defp receive_control(state, %{role: :provider} = route, sid, %{"type" => "renew"} = message) do
    if usable?(route) and valid_id?(message["nonce"]) and
         message["nonce"] != route.last_renewal and
         message["package_hash"] == route.scope["package_hash"] and
         message["lease_ms"] == route.lease_ms and
         not Enum.any?(state.effects, fn {_, {id, action}} ->
           id == route.id and match?({:validate_renewal, _, _}, action)
         end) do
      {ref, state} = effect(state, route.id, {:validate_renewal, message, sid})

      send(
        state.owner,
        {:arc_direct_validate, self(), ref, route.scope["capability"],
         route.scope["package_hash"]}
      )

      state
    else
      state
    end
  end

  defp receive_control(
         state,
         %{role: :caller, renewal: %{nonce: nonce, sid: expected_sid, started: started}} = route,
         sid,
         %{"type" => "renewed", "nonce" => nonce} = message
       ) do
    if usable?(route) and sid == expected_sid and message["lease_ms"] == route.lease_ms and
         message["package_hash"] == route.scope["package_hash"] do
      route = %{route | deadline: started + route.lease_ms, renewal: nil, relay_sid: sid}
      put_route(%{state | relay_up: true}, route) |> schedule_lease(route)
    else
      state
    end
  end

  defp receive_control(state, _route, _sid, _message), do: state

  defp control_result(state, route, :offer, :ok, sid),
    do: put_route(state, %{route | relay_sid: sid})

  defp control_result(state, route, :ready, :ok, _sid) do
    release(state, route)
    state
  end

  defp control_result(state, _route, _action, :ok, _sid), do: state

  defp control_result(state, route, action, _error, _sid) when action in [:renew, :renewed],
    do: retry_renewal(%{state | relay_up: false}, route.id)

  defp control_result(state, _route, :best_effort, _error, _sid), do: state

  defp control_result(state, route, _action, _error, _sid),
    do: retire(state, route.id, :relay_unavailable)

  defp accept_renewal(state, route, message, sid) do
    if usable?(route) do
      route = %{
        route
        | deadline: now() + route.lease_ms,
          relay_sid: sid,
          last_renewal: message["nonce"]
      }

      state = put_route(%{state | relay_up: true}, route) |> schedule_lease(route)

      control(
        state,
        route,
        %{
          "type" => "renewed",
          "nonce" => message["nonce"],
          "lease_ms" => route.lease_ms,
          "package_hash" => route.scope["package_hash"]
        },
        :renewed
      )
    else
      retire(state, route.id, :expired)
    end
  end

  defp retry_renewal(state, id) do
    case state.routes[id] do
      %{role: :caller} = route ->
        Process.send_after(self(), {:renew, id, route.deadline}, 250)
        put_route(%{state | relay_up: false}, %{route | renewal: nil})

      _ ->
        state
    end
  end

  defp new_route(id, peer, scope, rule, role, credentials, session) do
    %{
      id: id,
      peer: peer,
      scope: scope,
      rule: rule,
      role: role,
      credentials: credentials,
      session: session,
      received_seq: -1,
      peer_fingerprint: nil,
      relay_sid: nil,
      binding: nil,
      phase: :offered,
      deadline: now() + rule.lease_ms,
      lease_ms: rule.lease_ms,
      connections: %{},
      connected: MapSet.new(),
      selected: nil,
      prepared: nil,
      from: nil,
      package: nil,
      renewal: nil,
      last_renewal: nil,
      hole_punch: false,
      punch_worker: nil,
      punch_context: nil,
      peer_punch: nil,
      punch_started: false
    }
  end

  defp valid_punch_accept?(route, message) do
    enabled = Map.get(message, "hole_punch", false)
    is_boolean(enabled) and (not enabled or route.rule.hole_punch)
  end

  defp observe_punch_endpoint(state, %{hole_punch: false}), do: state

  defp observe_punch_endpoint(state, route) do
    owner = self()
    public_key = state.identity.public_key

    worker =
      spawn(fn ->
        result = network(:relay_endpoint, [public_key])
        send(owner, {:punch_endpoint, route.id, self(), result})
      end)

    put_route(state, %{route | punch_worker: worker})
  end

  defp punch_endpoint(state, route, {:ok, context}) do
    route = %{route | punch_context: context}
    state = put_route(state, start_punch(route, state))

    control(
      state,
      route,
      %{"type" => "punch_candidate", "candidate" => context.observed},
      :punch_candidate
    )
  end

  defp punch_endpoint(state, route, _error), do: put_route(state, route)

  defp start_punch(
         %{punch_started: false, punch_context: context, peer_punch: {host, port}} = route,
         state
       )
       when is_map(context) do
    owner = self()
    {local_ip, local_port} = context.local

    opts =
      carrier_opts(route, state, "punch") ++
        [
          host: host,
          port: port,
          local_ip: local_ip,
          local_port: local_port,
          role: if(route.role == :caller, do: :client, else: :server)
        ]

    worker =
      spawn(fn ->
        result =
          if network(:relay_endpoint_current?, [state.identity.public_key, context.connection]) ==
               true,
             do: carrier(:punch, [owner, opts]),
             else: {:error, :stale_relay_endpoint}

        send(owner, {:dial_result, route.id, "punch", self(), result})
      end)

    %{
      route
      | punch_started: true,
        connections: Map.put(route.connections, "punch", {:dialing, worker})
    }
  end

  defp start_punch(route, _state), do: route

  defp network(function, args) do
    if Code.ensure_loaded?(Arc.Net),
      do: apply(Arc.Net, function, args),
      else: {:error, :relay_runtime_unavailable}
  catch
    :exit, _ -> {:error, :relay_unavailable}
  end

  defp pending_connection?("punch", {:dialing, worker}), do: is_pid(worker)
  defp pending_connection?(_direction, pid), do: is_pid(pid)

  defp listen(%{rule: %{listen: nil}} = route, _state), do: {route, nil}

  defp listen(route, state) do
    listener = route.rule.listen
    direction = Atom.to_string(route.role)
    opts = carrier_opts(route, state, direction) ++ [ip: listener.ip, port: listener.port]

    case carrier(:listen, [self(), opts]) do
      {:ok, pid, port} ->
        {%{route | connections: Map.put(route.connections, direction, pid)},
         %{"host" => Policy.format_ip(listener.host), "port" => port}}

      _ ->
        {route, nil}
    end
  end

  defp dial(route, state, direction, candidate) do
    case Policy.candidate(route.rule, candidate) do
      {:ok, host, port} ->
        owner = self()
        opts = carrier_opts(route, state, direction) ++ [host: host, port: port]

        worker =
          spawn(fn ->
            result = carrier(:connect, [owner, opts])
            send(owner, {:dial_result, route.id, direction, self(), result})
          end)

        %{route | connections: Map.put(route.connections, direction, {:dialing, worker})}

      _ ->
        route
    end
  end

  defp valid_connection?({:dialing, _worker}, pid), do: is_pid(pid)
  defp valid_connection?(expected, pid), do: expected == pid

  defp carrier_opts(route, state, direction) do
    [
      identity: state.identity,
      credentials: route.credentials,
      peer_key: route.peer,
      peer_fingerprint: route.peer_fingerprint,
      binding: :crypto.hash(:sha256, route.binding <> direction),
      tag: {route.id, direction},
      timeout_ms: min(@attempt_ms, max(1, route.deadline - now()))
    ]
  end

  defp prepare(state, route) do
    if route.punch_worker, do: Process.exit(route.punch_worker, :shutdown)

    route.connections
    |> Enum.reject(fn {direction, _} -> direction == route.selected end)
    |> Enum.each(fn {_, pid} -> close_connection(pid) end)

    route = %{
      route
      | phase: :preparing,
        connections: Map.take(route.connections, [route.selected])
    }

    state = put_route(state, route)
    {ref, state} = effect(state, route.id, :prepare)
    send(state.owner, {:arc_direct_prepare, self(), ref, route.peer})
    state
  end

  defp arm_route(state, id, ref) do
    case state.routes[id] do
      route when is_map(route) ->
        if route.deadline > now() do
          route = %{route | prepared: ref}
          state = put_route(state, route)
          announce_ready(state, route)
        else
          retire(state, id, :busy)
        end

      _ ->
        state
    end
  end

  defp announce_ready(state, %{role: :caller} = route) do
    control(state, route, %{"type" => "nominate", "direction" => route.selected}, :nominate)
  end

  defp announce_ready(state, route) do
    # The receiver is armed before reporting readiness. Only the caller submits
    # requests on this service conversation.
    route = %{route | phase: :active}
    state = put_route(state, route) |> schedule_lease(route)
    control(state, route, %{"type" => "ready", "direction" => route.selected}, :ready)
  end

  defp receive_packet(state, route, packet) do
    with {:ok, decoded} <- Packet.decode(packet),
         true <- decoded.src == route.peer and decoded.dst == state.identity.public_key,
         true <-
           decoded.session_id == route.session.session_id and decoded.ek == route.session.ek_pub,
         true <- decoded.seq > route.received_seq,
         true <- abs(System.system_time(:millisecond) - decoded.ts) <= 120_000,
         {:ok, plaintext} <- Session.decrypt(route.session, decoded.nonce, decoded.ciphertext),
         {:ok, frame} <- Frame.decode(plaintext) do
      route = %{route | received_seq: decoded.seq}
      state = put_route(state, route)

      cond do
        frame.type == :event and frame.meta["topic"] == @topic ->
          if frame.body == json(%{"type" => "withdraw", "id" => route.id}),
            do: retire(state, route.id, :withdrawn),
            else: state

        allowed_frame?(route, frame, :incoming) ->
          case Process.info(state.owner, :message_queue_len) do
            {:message_queue_len, count} when count < 16 ->
              send(
                state.owner,
                {:arc_direct_frame, self(), route.id, route.peer, frame, route.deadline}
              )

              state

            _ ->
              retire(state, route.id, :owner_overloaded)
          end

        true ->
          retire(state, route.id, :invalid_direct_frame)
      end
    else
      _ -> retire(state, route.id, :invalid_direct_packet)
    end
  rescue
    _ -> retire(state, route.id, :invalid_direct_packet)
  end

  defp write_packet(state, route, payload) do
    if usable?(route) do
      {nonce, ciphertext, seq, session} = Session.encrypt(route.session, payload)

      packet =
        Packet.encode(state.identity, route.peer, session.session_id, seq, nonce, ciphertext,
          ek: session.ek_pub
        )

      state = put_route(state, %{route | session: session})

      case carrier(:send_packet, [
             route.connections[route.selected],
             packet,
             min(route.deadline, now() + 1_000)
           ]) do
        :ok -> {:ok, state}
        _ -> {:error, :outcome_unknown, retire(state, route.id, :direct_failed)}
      end
    else
      {:error, :direct_unavailable, state}
    end
  end

  defp allowed_frame?(route, %{type: :request, meta: meta, body: body}, direction) do
    request_direction? =
      (route.role == :caller and direction == :outgoing) or
        (route.role == :provider and direction == :incoming)

    request_direction? and byte_size(body) <= 1_048_576 and
      meta["path"] == route.scope["path"] and meta["capability_id"] == route.scope["capability"] and
      meta["method"] == route.scope["method"]
  end

  defp allowed_frame?(route, %{type: type, body: body}, direction)
       when type in [:response, :error] do
    byte_size(body) <= 1_048_576 and
      ((route.role == :provider and direction == :outgoing) or
         (route.role == :caller and direction == :incoming))
  end

  defp allowed_frame?(_, _, _), do: false

  defp valid_scope?(scope, caller, provider) when is_map(scope) and map_size(scope) == 10 do
    scope["version"] == 1 and scope["caller"] == hex(caller) and
      scope["provider"] == hex(provider) and
      is_binary(scope["capability"]) and
      Regex.match?(~r/\A[a-zA-Z0-9_-]{1,64}\z/, scope["capability"]) and
      match?({:ok, _}, unhex(scope["package_hash"], 32)) and
      is_binary(scope["method"]) and byte_size(scope["method"]) in 1..32 and
      match?(
        {:ok, _},
        Arc.Data.Protocol.parse("#{scope["scheme"]}+arc://#{scope["provider"]}#{scope["path"]}")
      )
  end

  defp valid_scope?(_, _, _), do: false

  defp package_matches?(route) do
    package = route.package
    cap = package["capability"]

    package["package_hash"] == route.scope["package_hash"] and
      cap["id"] == route.scope["capability"] and cap["scheme"] == route.scope["scheme"] and
      get_in(cap, ["invocation", "method"]) == route.scope["method"] and
      get_in(cap, ["invocation", "mode"]) == "request_reply"
  end

  defp scope_query(target, capability),
    do: %{"capability" => capability, "scheme" => target.scheme, "path" => target.path}

  defp scope_matches?(a, b), do: Enum.all?(b, fn {key, value} -> a[key] == value end)

  defp context_binding(route) do
    {caller_fp, provider_fp} =
      if route.role == :caller,
        do: {route.credentials.fingerprint, route.peer_fingerprint},
        else: {route.peer_fingerprint, route.credentials.fingerprint}

    fields =
      Enum.map(
        ~w(version caller provider capability scheme path package_hash method session ek),
        &route.scope[&1]
      )

    # Keep the existing binding unchanged for peers without the extension.
    fields = if route.hole_punch, do: fields ++ ["tcp-hole-punch-v1"], else: fields

    :crypto.hash(
      :sha256,
      "ARC_DIRECT_V1" <>
        json([
          route.id,
          hex(route.relay_sid),
          fields,
          hex(caller_fp),
          hex(provider_fp),
          route.lease_ms
        ])
    )
  end

  defp public_route(route),
    do: %{generation: route.id, package: route.package, deadline: route.deadline}

  defp usable?(%{phase: :active, deadline: deadline}), do: now() < deadline
  defp usable?(_), do: false
  defp put_route(state, route), do: %{state | routes: Map.put(state.routes, route.id, route)}

  defp room?(state),
    do: map_size(state.routes) < @max_routes and map_size(state.tombstones) < @max_tombstones

  defp busy?(state, peer, scope),
    do:
      Enum.any?(state.routes, fn {_, route} ->
        route.peer == peer and
          scope_matches?(route.scope, Map.take(scope, ~w(capability scheme path)))
      end)

  defp schedule_lease(state, route) do
    Process.send_after(
      self(),
      {:lease_expired, route.id, route.deadline},
      max(0, route.deadline - now())
    )

    if route.role == :caller do
      Process.send_after(
        self(),
        {:renew, route.id, route.deadline},
        max(1, div(route.deadline - now(), 2))
      )
    end

    state
  end

  defp control(state, route, message, action) do
    {ref, state} = effect(state, route.id, action)

    send(
      state.owner,
      {:arc_direct_control, self(), ref, route.peer, Map.put(message, "id", route.id)}
    )

    state
  end

  defp effect(state, id, action) do
    ref = make_ref()
    {ref, %{state | effects: Map.put(state.effects, ref, {id, action})}}
  end

  defp pop_effect(state, ref) do
    {value, effects} = Map.pop(state.effects, ref)
    {value, %{state | effects: effects}}
  end

  defp retire(state, id, reason) do
    case Map.pop(state.routes, id) do
      {nil, _} ->
        state

      {route, remaining} ->
        if route.from, do: GenServer.reply(route.from, {:error, reason})
        release(state, route)
        close_connections(route)
        effects = Map.reject(state.effects, fn {_, {generation, _}} -> generation == id end)
        key = {route.peer, Map.take(route.scope, ~w(capability scheme path))}

        %{
          state
          | routes: remaining,
            effects: effects,
            tombstones:
              Map.put(state.tombstones, id, %{
                deadline: now() + @tombstone_ms,
                peer: route.peer,
                session_id: route.session.session_id
              }),
            cooldowns: Map.put(state.cooldowns, key, now() + @cooldown_ms)
        }
    end
  end

  defp release(state, route) do
    if route.prepared,
      do: send(state.owner, {:arc_direct_release, self(), route.prepared, route.peer})

    # A prepare effect can still be waiting in the agent's mailbox on timeout.
    Enum.each(state.effects, fn
      {ref, {id, :prepare}} when id == route.id ->
        send(state.owner, {:arc_direct_release, self(), ref, route.peer})

      _ ->
        :ok
    end)
  end

  defp close_connections(route) do
    if route.punch_worker, do: Process.exit(route.punch_worker, :shutdown)
    Enum.each(route.connections, fn {_, pid} -> close_connection(pid) end)
  end

  defp close_connection({:dialing, worker}), do: Process.exit(worker, :shutdown)
  defp close_connection(pid), do: carrier(:close, [pid])

  defp sweep(state),
    do: %{
      state
      | tombstones: Map.reject(state.tombstones, fn {_, entry} -> entry.deadline <= now() end),
        cooldowns: Map.reject(state.cooldowns, fn {_, deadline} -> deadline <= now() end)
    }

  defp carrier(function, args) do
    if Code.ensure_loaded?(Arc.Net.Direct),
      do: apply(Arc.Net.Direct, function, args),
      else: {:error, :direct_runtime_unavailable}
  catch
    :exit, _ -> {:error, :direct_unavailable}
  end

  defp unhex(value, bytes) when is_binary(value) and byte_size(value) == bytes * 2,
    do: Base.decode16(value, case: :mixed)

  defp unhex(_, _), do: {:error, :invalid_hex}
  defp valid_id?(id), do: match?({:ok, _}, unhex(id, 16))
  defp random_id, do: hex(:crypto.strong_rand_bytes(16))
  defp hex(bytes), do: Base.encode16(bytes, case: :lower)
  defp json(value), do: value |> :json.encode() |> IO.iodata_to_binary()
  defp now, do: System.monotonic_time(:millisecond)
end
