// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {SecBase} from "./Base.sol";
import {Test} from "forge-std/Test.sol";
import {XmrSwapV3} from "../../src/XmrSwapV3.sol";
import {SeshRegistry} from "../../src/registry/SeshRegistry.sol";
import {ExitLease} from "../../src/exits/ExitLease.sol";
import {MockUSDC} from "../../src/MockUSDC.sol";

contract RejectAll {
    receive() external payable { revert(); }
}

/// ETH receiver that tries to re-enter the registry / lease on every payment.
contract Reenterer {
    SeshRegistry public reg;
    ExitLease public el;
    uint256 public id;
    bool public reentered;
    constructor(SeshRegistry r, ExitLease e) { reg = r; el = e; }
    function setId(uint256 i) external { id = i; }
    function challengeOn(uint256 i) external payable { reg.challenge{value: msg.value}(i); }
    receive() external payable {
        // Every attempt must fail; a success is recorded.
        try reg.withdrawOwed(address(this)) { reentered = true; } catch {}
        try reg.resolveChallenge(id) { reentered = true; } catch {}
        try reg.sweepRemoved(id) { reentered = true; } catch {}
        try el.buy{value: 0}(id, address(0), 1, 0, "") { reentered = true; } catch {}
    }
}

/// Random sequences over SeshRegistry (register / updateMeta / topUp /
/// requestExit / cancelExit / withdraw / challenge / answer / resolve / sweep /
/// commit / ack / release / slash / withdrawOwed) and ExitLease
/// (setOffer / clearOffer / buy in ETH and USDC), from owners, attackers and
/// a re-entering contract.
contract RegLeaseHandler is Test {
    uint256 constant BOB_K = 0xd51fb7a1b6932d11ee69a0e977c3c3bc0f3be706735daf55f6c11050b5c0c96b;
    uint256 constant ALICE_S = 0x0a4c454746ce6afc23d46643a0f26aba063b6af57206da911d09552be6b47a25;
    uint256 constant ALICE_K = 0x01c2a86a450bfac25beed06f52bc6b0ce204bcff0671285c93269f341c8e0254;

    SeshRegistry public reg;
    ExitLease public el;
    XmrSwapV3 public v3;
    MockUSDC public usdc;

    uint256[3] public pks = [uint256(0x1111), 0x2222, 0x3333];
    address[] public owners;
    address public attacker = address(0xBAD);
    address public taker = address(0x7A4E2);
    address public rejecter;
    Reenterer public reenter;
    address[] public payouts;

    // ghosts
    uint256 public ghostChallenges; // open challenges (challengeBond held)
    mapping(uint256 => address) public listingOwner;
    bool public nonOwnerActed;
    bool public withdrewEarly;
    bool public leaseOnDeadListing;
    bool public leaseWrongPay;
    bool public answerAndResolveSameTime;
    bool public bondMoved; // a bond changed in a way no rule allows
    uint256 public nLeases;
    uint256 public nCommits;
    uint256 public nSlashes;
    uint256 public nWithdraws;
    uint256 public nResolves;
    uint256 public nAnswers;
    uint256 salt;

    struct C { bytes32 id; XmrSwapV3.Swap s; uint256 lid; }
    C[] public commits;
    address[] public owedWho;

    string constant BSESH = "bog6mtasrhyriq5b35qidik4e3earg9om8d989hhza3jaeb5oqwo.sesh";

    constructor(SeshRegistry r, ExitLease e, XmrSwapV3 v, MockUSDC u) {
        reg = r; el = e; v3 = v; usdc = u;
        for (uint256 i = 0; i < 3; i++) { owners.push(vm.addr(pks[i])); vm.deal(owners[i], 1e24); }
        vm.deal(attacker, 1e24);
        vm.deal(taker, 1e24);
        rejecter = address(new RejectAll());
        reenter = new Reenterer(r, e);
        vm.deal(address(reenter), 1e24);
        payouts.push(address(0x9A70));
        payouts.push(rejecter);
        payouts.push(address(reenter));
        vm.prank(taker); usdc.approve(address(e), type(uint256).max);
        vm.prank(attacker); usdc.approve(address(e), type(uint256).max);
        owedWho.push(rejecter); owedWho.push(address(reenter)); owedWho.push(taker); owedWho.push(attacker);
        for (uint256 i = 0; i < 3; i++) owedWho.push(owners[i]);
        // Seed one listing of each kind so short sequences still reach deep states.
        _seed(0, "maker", 0.01 ether, "seedm.sesh");
        _seed(1, "exit", 0.001 ether, "seede.sesh");
        _seed(2, "site", 0.001 ether, "seeds.sesh");
        vm.prank(owners[1]);
        e.setOffer(2, payouts[0], 1e12, 100_000, 744);
    }

    function _seed(uint256 who, bytes32 tag, uint256 bond, string memory name) internal {
        vm.prank(owners[who]);
        uint256 id = reg.register{value: bond}(name, tag, "seed");
        listingOwner[id] = owners[who];
    }

    function _id(uint256 i) internal view returns (uint256) {
        uint256 n = reg.listingCount();
        return n == 0 ? 0 : 1 + (i % (n + 1)); // includes one nonexistent id
    }

    function _actor(uint256 i, uint256 lid) internal view returns (address) {
        uint256 k = i % 5;
        if (k < 3) return owners[k];
        if (k == 3) return attacker;
        address o = listingOwner[lid]; // bias toward the right owner
        return o == address(0) ? attacker : o; // msg.sender can never be address(0)
    }

    function _isOwner(address a, uint256 lid) internal view returns (bool) {
        return listingOwner[lid] != address(0) && a == listingOwner[lid];
    }

    function warp(uint256 dt) external { vm.warp(block.timestamp + bound(dt, 0, 4 days)); }

    function warpExact(uint256 i, bool toChallenge) external {
        uint256 lid = _id(i);
        if (lid == 0) return;
        SeshRegistry.Listing memory l = reg.listing(lid);
        uint64 t = toChallenge ? l.challengeDeadline : l.exitAt;
        if (t > block.timestamp) vm.warp(t);
    }

    function register(uint256 who, uint256 tagSel, uint256 bond, uint256 nameSel) external {
        address a = owners[who % 3];
        bytes32 tag = tagSel % 3 == 0 ? bytes32("maker") : tagSel % 3 == 1 ? bytes32("exit") : bytes32("site");
        bond = bound(bond, SITE_BOND_() , 0.05 ether);
        string memory name = string.concat("n", vm.toString(nameSel % 8), ".sesh");
        vm.prank(a);
        try reg.register{value: bond}(name, tag, "m") returns (uint256 id) {
            listingOwner[id] = a;
        } catch {}
    }

    function SITE_BOND_() internal view returns (uint256) { return reg.siteMinBond(); }

    function updateMeta(uint256 i, uint256 who) external {
        uint256 lid = _id(i); address a = _actor(who, lid);
        vm.prank(a);
        try reg.updateMeta(lid, "x") { if (!_isOwner(a, lid)) nonOwnerActed = true; } catch {}
    }

    function topUp(uint256 i, uint256 amt) external {
        uint256 lid = _id(i);
        amt = bound(amt, 0, 0.01 ether);
        vm.prank(attacker);
        try reg.topUp{value: amt}(lid) {} catch {}
    }

    function requestExit(uint256 i, uint256 who) external {
        uint256 lid = _id(i); address a = _actor(who, lid);
        vm.prank(a);
        try reg.requestExit(lid) { if (!_isOwner(a, lid)) nonOwnerActed = true; } catch {}
    }

    function cancelExit(uint256 i, uint256 who) external {
        uint256 lid = _id(i); address a = _actor(who, lid);
        vm.prank(a);
        try reg.cancelExit(lid) { if (!_isOwner(a, lid)) nonOwnerActed = true; } catch {}
    }

    function withdraw(uint256 i, uint256 who, bool toRejecter) external {
        uint256 lid = _id(i); address a = _actor(who, lid);
        SeshRegistry.Listing memory l = reg.listing(lid);
        address to = toRejecter ? rejecter : a;
        uint256 b0 = to.balance;
        uint256 o0 = reg.owed(to);
        vm.prank(a);
        try reg.withdraw(lid, to) {
            nWithdraws++;
            if (!_isOwner(a, lid)) nonOwnerActed = true;
            if (block.timestamp < l.exitAt || l.challenger != address(0) || l.openCommits != 0) withdrewEarly = true;
            if (to.balance - b0 + reg.owed(to) - o0 != l.bond) bondMoved = true;
        } catch {}
    }

    function challenge(uint256 i, uint256 who) external {
        uint256 lid = _id(i);
        uint256 cb = reg.challengeBond();
        if (who % 3 == 0) {
            reenter.setId(lid);
            try reenter.challengeOn{value: cb}(lid) { ghostChallenges++; } catch {}
            return;
        }
        address a = _actor(who, lid);
        vm.prank(a);
        try reg.challenge{value: cb}(lid) {
            ghostChallenges++;
            if (_isOwner(a, lid)) nonOwnerActed = true; // owner may not challenge itself
        } catch {}
    }

    function answer(uint256 i, uint256 who) external {
        uint256 lid = _id(i); address a = _actor(who, lid);
        SeshRegistry.Listing memory l = reg.listing(lid);
        vm.prank(a);
        try reg.answer(lid) {
            nAnswers++;
            ghostChallenges--;
            if (!_isOwner(a, lid)) nonOwnerActed = true;
            if (block.timestamp >= l.challengeDeadline) answerAndResolveSameTime = true;
        } catch {}
    }

    function resolve(uint256 i) external {
        uint256 lid = _id(i);
        SeshRegistry.Listing memory l = reg.listing(lid);
        vm.prank(attacker);
        try reg.resolveChallenge(lid) {
            nResolves++;
            ghostChallenges--;
            if (block.timestamp < l.challengeDeadline) answerAndResolveSameTime = true;
        } catch {}
    }

    function sweep(uint256 i) external {
        uint256 lid = _id(i);
        vm.prank(attacker);
        try reg.sweepRemoved(lid) {} catch {}
    }

    function withdrawOwed(uint256 w) external {
        address a = owedWho[w % owedWho.length];
        if (a == rejecter || a == address(reenter)) return; // contracts: no pull path from here
        vm.prank(a);
        try reg.withdrawOwed(a) {} catch {}
    }

    // ---------------------------------------------------------------- maker commitments
    function commitSwap(uint256 i, uint256 value, bool attackerSigns) external {
        uint256 lid = _id(i);
        if (lid == 0) return;
        value = bound(value, 1, 0.01 ether);
        address claimer = address(uint160(0xC000 + (++salt)));
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(
            taker, claimer, BOB_K, ALICE_K, uint64(block.timestamp) + 1 hours, uint64(block.timestamp) + 2 hours,
            30 minutes, address(0), value, 0, bytes32(salt)
        );
        vm.prank(taker);
        bytes32 id = v3.lock{value: value}(claimer, BOB_K, ALICE_K, 1 hours, 1 hours, 30 minutes, address(0), value, 0, s.salt);
        uint256 pen = reg.requiredPenalty(address(0), value);
        SeshRegistry.Commitment memory c = SeshRegistry.Commitment(
            lid, taker, claimer, BOB_K, address(0), value, 1, pen, 10 minutes, uint64(block.timestamp) + 15 minutes
        );
        uint256 pk = attackerSigns ? 0xBAD : _pkOf(listingOwner[lid]);
        if (pk == 0) return;
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(pk, reg.commitmentDigest(c));
        try reg.commit(s, c, v, r, ss) {
            nCommits++;
            commits.push(C(id, s, lid));
            if (attackerSigns && listingOwner[lid] != vm.addr(0xBAD)) nonOwnerActed = true;
        } catch {}
    }

    function _pkOf(address a) internal view returns (uint256) {
        for (uint256 i = 0; i < 3; i++) if (owners[i] == a) return pks[i];
        return 0;
    }

    function ackCommit(uint256 i, bool attackerSigns) external {
        if (commits.length == 0) return;
        C memory c = commits[i % commits.length];
        uint256 pk = attackerSigns ? 0xBAD : _pkOf(listingOwner[c.lid]);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(pk, reg.ackDigest(c.id, c.lid));
        try reg.ack(c.id, v, r, ss) { if (attackerSigns) nonOwnerActed = true; } catch {}
    }

    function releaseCommit(uint256 i) external {
        if (commits.length == 0) return;
        C memory c = commits[i % commits.length];
        try reg.release(c.id) {} catch {}
    }

    function slashCommit(uint256 i) external {
        if (commits.length == 0) return;
        C memory c = commits[i % commits.length];
        (,uint64 attestBy,,,) = reg.commits(c.id);
        try reg.slash(c.id) {
            nSlashes++;
            if (block.timestamp < attestBy) withdrewEarly = true;
        } catch {}
    }

    function v3Step(uint256 i, uint256 op) external {
        if (commits.length == 0) return;
        C memory c = commits[i % commits.length];
        vm.startPrank(taker);
        if (op % 3 == 0) { try v3.setReady(c.s) {} catch {} }
        else if (op % 3 == 1) { try v3.cancelIntent(c.s) {} catch {} }
        else { try v3.refund(c.s, ALICE_S) {} catch {} }
        vm.stopPrank();
    }

    // ---------------------------------------------------------------- ExitLease
    function setOffer(uint256 i, uint256 who, uint256 p, uint128 wph, uint128 uph, uint32 mh) external {
        uint256 lid = _id(i); address a = _actor(who, lid);
        address payout = payouts[p % payouts.length];
        wph = uint128(bound(wph, 0, 1e15));
        uph = uint128(bound(uph, 0, 1e9));
        mh = uint32(bound(mh, 0, 800));
        vm.prank(a);
        try el.setOffer(lid, payout, wph, uph, mh) { if (!_isOwner(a, lid)) nonOwnerActed = true; } catch {}
    }

    function clearOffer(uint256 i, uint256 who) external {
        uint256 lid = _id(i); address a = _actor(who, lid);
        vm.prank(a);
        try el.clearOffer(lid) { if (!_isOwner(a, lid)) nonOwnerActed = true; } catch {}
    }

    function buy(uint256 i, bool useUsdc, uint32 h, bool byAttacker) external {
        uint256 lid = _id(i);
        address b = byAttacker ? attacker : taker;
        address asset = useUsdc ? address(usdc) : address(0);
        h = uint32(bound(h, 1, 800));
        uint256 q;
        try el.quote(lid, asset, h) returns (uint256 x) { q = x; } catch { return; }
        (, address payout,,,) = el.offers(lid);
        SeshRegistry.Info memory inf = reg.info(lid);
        uint256 pb0 = useUsdc ? usdc.balanceOf(payout) : payout.balance;
        uint64 until0 = el.paidUntil(lid, keccak256(bytes(BSESH)));
        vm.prank(b);
        try el.buy{value: useUsdc ? 0 : q}(lid, asset, h, q, BSESH) {
            nLeases++;
            if (inf.status != SeshRegistry.Status.ACTIVE || inf.tag != bytes32("exit")) leaseOnDeadListing = true;
            uint256 pb1 = useUsdc ? usdc.balanceOf(payout) : payout.balance;
            if (pb1 - pb0 != q) leaseWrongPay = true;
            uint64 until1 = el.paidUntil(lid, keccak256(bytes(BSESH)));
            uint64 base = until0 > block.timestamp ? until0 : uint64(block.timestamp);
            if (until1 != base + uint64(h) * 1 hours) leaseWrongPay = true;
        } catch {}
    }

    // ---------------------------------------------------------------- views for invariants
    function sumBonds() external view returns (uint256 s) {
        uint256 n = reg.listingCount();
        for (uint256 i = 1; i <= n; i++) s += reg.listing(i).bond;
    }

    function sumOwed() external view returns (uint256 s) {
        for (uint256 i = 0; i < owedWho.length; i++) s += reg.owed(owedWho[i]);
    }

    function commitCount() external view returns (uint256) { return commits.length; }
    function commitAt(uint256 i) external view returns (bytes32, uint256) { return (commits[i].id, commits[i].lid); }
}

contract RegistryLeaseInvariants is SecBase {
    XmrSwapV3 v3;
    SeshRegistry reg;
    ExitLease el;
    MockUSDC usdc;
    RegLeaseHandler h;

    function setUp() public {
        vm.warp(1_000_000);
        (v3, reg, el, usdc) = deployAll();
        h = new RegLeaseHandler(reg, el, v3, usdc);
        usdc.mint(h.taker(), 1e30);
        usdc.mint(h.attacker(), 1e30);
        targetContract(address(h));
    }

    function afterInvariant() public {
        if (vm.envOr("SEC_STATS", false)) {
            vm.writeLine("fuzz-stats/registry.csv", string.concat(
                vm.toString(reg.listingCount()), ",", vm.toString(h.nLeases()), ",", vm.toString(h.nCommits()), ",",
                vm.toString(h.nSlashes()), ",", vm.toString(h.nWithdraws()), ",", vm.toString(h.nAnswers()), ",",
                vm.toString(h.nResolves())));
        }
        emit log_named_uint("listings", reg.listingCount());
        emit log_named_uint("leases", h.nLeases());
        emit log_named_uint("commits", h.nCommits());
        emit log_named_uint("slashes", h.nSlashes());
        emit log_named_uint("withdraws", h.nWithdraws());
        emit log_named_uint("answers", h.nAnswers());
        emit log_named_uint("resolves", h.nResolves());
    }

    /// Registry holds exactly: every bond + every deferred payout + open challenge bonds.
    function invariant_RegistrySolvency() public view {
        assertEq(address(reg).balance, h.sumBonds() + h.sumOwed() + h.ghostChallenges() * reg.challengeBond());
    }

    /// ExitLease never holds funds.
    function invariant_LeaseHoldsNothing() public view {
        assertEq(address(el).balance, 0);
        assertEq(usdc.balanceOf(address(el)), 0);
    }

    /// Per listing: reserved <= bond, reserved == sum of OPEN penalties, openCommits == number OPEN.
    function invariant_ReservedMatchesOpenCommits() public view {
        uint256 n = reg.listingCount();
        for (uint256 lid = 1; lid <= n; lid++) {
            SeshRegistry.Listing memory l = reg.listing(lid);
            assertLe(l.reserved, l.bond);
            uint256 res; uint256 open;
            for (uint256 j = 0; j < h.commitCount(); j++) {
                (bytes32 id, uint256 cl) = h.commitAt(j);
                if (cl != lid) continue;
                (SeshRegistry.CommitStage st,,, , uint256 pen) = reg.commits(id);
                if (st == SeshRegistry.CommitStage.OPEN) { res += pen; open++; }
            }
            assertEq(l.reserved, res);
            assertEq(l.openCommits, open);
            // name index: points here only while ACTIVE / EXITING
            uint256 idx = reg.idOfSesh(keccak256(bytes(l.sesh)));
            if (l.status == SeshRegistry.Status.ACTIVE || l.status == SeshRegistry.Status.EXITING) assertEq(idx, lid);
            else assertTrue(idx != lid);
            if (l.status == SeshRegistry.Status.WITHDRAWN) assertEq(l.bond, 0);
        }
    }

    /// Only the listing owner can manage it; the owner cannot challenge itself; forged signatures never pass.
    function invariant_OwnerChecks() public view { assertTrue(!h.nonOwnerActed()); }

    /// No withdraw before exitAt / with a challenge or open commitment; no slash before attestBy.
    function invariant_NoEarlyExit() public view { assertTrue(!h.withdrewEarly()); assertTrue(!h.bondMoved()); }

    /// answer and resolveChallenge are exclusive at the exact deadline.
    function invariant_ChallengeBoundary() public view { assertTrue(!h.answerAndResolveSameTime()); }

    /// A lease is only sold on an ACTIVE exit listing, pays exactly the quote to the payout, stacks hours exactly.
    function invariant_LeasePaysRightParty() public view {
        assertTrue(!h.leaseOnDeadListing());
        assertTrue(!h.leaseWrongPay());
    }
}
