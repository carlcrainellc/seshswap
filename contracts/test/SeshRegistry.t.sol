// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {Test} from "forge-std/Test.sol";
import {XmrSwapV3} from "../src/XmrSwapV3.sol";
import {SeshRegistry} from "../src/registry/SeshRegistry.sol";
import {MockUSDC} from "../src/MockUSDC.sol";
import {MockAggregatorV3} from "../src/MockAggregatorV3.sol";

contract RejectEth {
    receive() external payable {
        revert();
    }

    function registerOn(SeshRegistry r, string calldata n) external payable returns (uint256) {
        return r.register{value: msg.value}(n, "site", "");
    }

    function exit(SeshRegistry r, uint256 id) external {
        r.requestExit(id);
    }

    function withdrawTo(SeshRegistry r, uint256 id, address to) external {
        r.withdraw(id, to);
    }
}

/// SeshRegistry: bonds, the maker quote cap, commit/ack/slash against a real
/// XmrSwapV3 lock, challenges, and the exit delay. Amounts are the testnet
/// placeholders the deploy script uses.
contract SeshRegistryTest is Test {
    XmrSwapV3 v3;
    SeshRegistry reg;
    MockUSDC usdc;
    MockAggregatorV3 ethUsd;

    uint256 makerPk = 0x3A4E5;
    address maker;
    address alice = address(0xA11CE); // taker
    address carol = address(0xCA201); // site owner
    address dave = address(0xDA7E); // challenger / watchtower
    uint256 constant BOB_K = 0xd51fb7a1b6932d11ee69a0e977c3c3bc0f3be706735daf55f6c11050b5c0c96b;
    uint256 constant ALICE_K = 0x01c2a86a450bfac25beed06f52bc6b0ce204bcff0671285c93269f341c8e0254;
    address claimer = address(0xC1A1);
    uint64 constant D1 = 1 hours;
    uint64 constant D2 = 1 hours;
    uint64 constant G = 30 minutes;

    // PLACEHOLDER testnet amounts (same as script/DeployRegistry.s.sol)
    uint256 constant SITE_BOND = 0.0002 ether;
    uint256 constant MAKER_BOND = 0.002 ether;
    uint256 constant CAP = 5; // max quote = bond * 5
    uint16 constant PEN_BPS = 500; // 5% of the swap's ETH value
    uint256 constant MIN_PEN = 0.00005 ether;
    uint256 constant CH_BOND = 0.0001 ether;
    uint64 constant EXIT_DELAY = 3 days;
    uint64 constant CH_WINDOW = 2 days;
    uint64 constant ATTEST = 10 minutes;

    string constant MSESH = "bog6mtasrhyriq5b35qidik4e3earg9om8d989hhza3jaeb5oqwo.sesh";

    function setUp() public {
        vm.warp(1_000_000);
        v3 = new XmrSwapV3();
        usdc = new MockUSDC();
        ethUsd = new MockAggregatorV3("ETH / USD", 3000e8);
        reg = new SeshRegistry(
            SeshRegistry.Params({
                swap: address(v3),
                siteMinBond: SITE_BOND,
                makerMinBond: MAKER_BOND,
                capFactor: CAP,
                penaltyBps: PEN_BPS,
                minPenalty: MIN_PEN,
                challengeBond: CH_BOND,
                exitDelay: EXIT_DELAY,
                challengeWindow: CH_WINDOW,
                minAttest: ATTEST,
                maxAttest: 30 minutes,
                usdc: address(usdc),
                usdcDecimals: 6,
                ethUsdFeed: address(ethUsd),
                maxFeedAge: 2 days
            })
        );
        maker = vm.addr(makerPk);
        vm.deal(maker, 10 ether);
        vm.deal(alice, 10 ether);
        vm.deal(carol, 1 ether);
        vm.deal(dave, 1 ether);
    }

    // ------------------------------------------------------------ helpers
    function listMaker(uint256 bond) internal returns (uint256 id) {
        vm.prank(maker);
        id = reg.register{value: bond}(MSESH, "maker", "ETH->XMR, Arbitrum Sepolia, test maker");
    }

    function listSite() internal returns (uint256 id) {
        vm.prank(carol);
        id = reg.register{value: SITE_BOND}("carolsforum.sesh", "forum", "a test forum");
    }

    function lockEth(uint256 value, bytes32 salt) internal returns (XmrSwapV3.Swap memory s, bytes32 id) {
        s = XmrSwapV3.Swap(
            alice,
            claimer,
            BOB_K,
            ALICE_K,
            uint64(block.timestamp) + D1,
            uint64(block.timestamp) + D1 + D2,
            G,
            address(0),
            value,
            0,
            salt
        );
        vm.prank(alice);
        id = v3.lock{value: value}(claimer, BOB_K, ALICE_K, D1, D2, G, address(0), value, 0, salt);
    }

    function commitment(uint256 lid, XmrSwapV3.Swap memory s, uint256 penalty)
        internal
        view
        returns (SeshRegistry.Commitment memory)
    {
        return SeshRegistry.Commitment(
            lid,
            s.owner,
            s.claimer,
            s.claimKey,
            s.asset,
            s.value,
            3_663_126_998,
            penalty,
            ATTEST,
            uint64(block.timestamp) + 15 minutes
        );
    }

    function signC(uint256 pk, SeshRegistry.Commitment memory c) internal view returns (uint8 v, bytes32 r, bytes32 s) {
        return vm.sign(pk, reg.commitmentDigest(c));
    }

    function doCommit(uint256 lid, XmrSwapV3.Swap memory s, uint256 penalty) internal {
        SeshRegistry.Commitment memory c = commitment(lid, s, penalty);
        (uint8 v, bytes32 r, bytes32 ss) = signC(makerPk, c);
        vm.prank(alice);
        reg.commit(s, c, v, r, ss);
    }

    function stage(bytes32 id) internal view returns (SeshRegistry.CommitStage st) {
        (st,,,,,) = reg.commits(id);
    }

    // ------------------------------------------------------------ bonds and listings
    function test_RegisterSiteAndMakerWithBonds() public {
        vm.expectEmit(true, true, true, true);
        emit SeshRegistry.Listed(1, maker, "maker", MSESH, "ETH->XMR, Arbitrum Sepolia, test maker", MAKER_BOND);
        uint256 m = listMaker(MAKER_BOND);
        uint256 s = listSite();
        assertEq(m, 1);
        assertEq(s, 2);
        assertEq(reg.listing(s).bond, SITE_BOND);
        assertEq(uint256(reg.listing(m).status), uint256(SeshRegistry.Status.ACTIVE));
        assertEq(reg.idOfSesh(keccak256(bytes(MSESH))), m);
        assertEq(address(reg).balance, MAKER_BOND + SITE_BOND);
    }

    function test_MakerNeedsLargerBond() public {
        vm.prank(maker);
        vm.expectRevert(SeshRegistry.LowBond.selector);
        reg.register{value: SITE_BOND}(MSESH, "maker", "");
        // the same amount is enough for a site
        vm.prank(maker);
        reg.register{value: SITE_BOND}(MSESH, "site", "");
    }

    function test_NoExactDuplicatesAndNameRules() public {
        listMaker(MAKER_BOND);
        vm.prank(carol);
        vm.expectRevert(SeshRegistry.Taken.selector);
        reg.register{value: SITE_BOND}(MSESH, "site", "");
        string[6] memory bad = ["Upper.sesh", "a.b.sesh", "-x.sesh", "x-.sesh", ".sesh", "name.onion"];
        for (uint256 i; i < bad.length; i++) {
            vm.prank(carol);
            vm.expectRevert(SeshRegistry.BadName.selector);
            reg.register{value: SITE_BOND}(bad[i], "site", "");
        }
        vm.prank(carol);
        reg.register{value: SITE_BOND}("ok-name-1.sesh", "site", "");
    }

    function test_TopUpAndMetaUpdate() public {
        uint256 id = listMaker(MAKER_BOND);
        vm.prank(dave); // anyone may top up
        reg.topUp{value: 0.001 ether}(id);
        assertEq(reg.listing(id).bond, MAKER_BOND + 0.001 ether);
        vm.prank(dave);
        vm.expectRevert(SeshRegistry.NotOwner.selector);
        reg.updateMeta(id, "spam");
        vm.prank(maker);
        reg.updateMeta(id, "new text");
        assertEq(reg.listing(id).meta, "new text");
    }

    // ------------------------------------------------------------ quote cap
    function test_QuoteCapIsBondTimesFactor() public {
        uint256 id = listMaker(MAKER_BOND);
        assertEq(reg.maxQuoteWei(id), MAKER_BOND * CAP); // 0.01 ETH
        // exactly at the cap: ok
        (XmrSwapV3.Swap memory s,) = lockEth(MAKER_BOND * CAP, bytes32(uint256(1)));
        doCommit(id, s, reg.requiredPenalty(address(0), s.value));
        // one wei over: refused
        (XmrSwapV3.Swap memory s2,) = lockEth(MAKER_BOND * CAP + 1, bytes32(uint256(2)));
        SeshRegistry.Commitment memory c = commitment(id, s2, 0.001 ether);
        (uint8 v, bytes32 r, bytes32 ss) = signC(makerPk, c);
        vm.expectRevert(SeshRegistry.OverCap.selector);
        reg.commit(s2, c, v, r, ss);
        // top up raises the cap
        vm.prank(maker);
        reg.topUp{value: MAKER_BOND}(id);
        reg.commit(s2, c, v, r, ss);
    }

    function test_UsdcCapUsesEthUsdFeed() public {
        uint256 id = listMaker(MAKER_BOND);
        // 0.01 ETH cap at $3000 = $30
        assertEq(reg.ethValue(address(usdc), 30e6), 0.01 ether);
        usdc.mint(alice, 100e6);
        vm.prank(alice);
        usdc.approve(address(v3), type(uint256).max);
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(
            alice,
            claimer,
            BOB_K,
            ALICE_K,
            uint64(block.timestamp) + D1,
            uint64(block.timestamp) + D1 + D2,
            G,
            address(usdc),
            31e6,
            0,
            bytes32(uint256(9))
        );
        vm.prank(alice);
        v3.lock(claimer, BOB_K, ALICE_K, D1, D2, G, address(usdc), 31e6, 0, bytes32(uint256(9)));
        SeshRegistry.Commitment memory c = commitment(id, s, 0.001 ether);
        (uint8 v, bytes32 r, bytes32 ss) = signC(makerPk, c);
        vm.expectRevert(SeshRegistry.OverCap.selector);
        reg.commit(s, c, v, r, ss);
        // stale feed: refused
        vm.warp(block.timestamp + 3 days);
        vm.expectRevert(SeshRegistry.StaleFeed.selector);
        reg.ethValue(address(usdc), 1e6);
    }

    // ------------------------------------------------------------ commit / ack / slash
    function test_CommitNeedsRealV3LockAndMatchingTerms() public {
        uint256 id = listMaker(MAKER_BOND);
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(
            alice,
            claimer,
            BOB_K,
            ALICE_K,
            uint64(block.timestamp) + D1,
            uint64(block.timestamp) + D1 + D2,
            G,
            address(0),
            0.001 ether,
            0,
            bytes32(uint256(5))
        );
        SeshRegistry.Commitment memory c = commitment(id, s, MIN_PEN);
        (uint8 v, bytes32 r, bytes32 ss) = signC(makerPk, c);
        vm.expectRevert(SeshRegistry.NotLocked.selector);
        reg.commit(s, c, v, r, ss);
        (s,) = lockEth(0.001 ether, bytes32(uint256(5)));
        // a different claimer than the maker signed for
        SeshRegistry.Commitment memory bad = commitment(id, s, MIN_PEN);
        bad.claimer = address(0xBAD);
        (v, r, ss) = signC(makerPk, bad);
        vm.expectRevert(SeshRegistry.BadParams.selector);
        reg.commit(s, bad, v, r, ss);
        // signed by someone other than the listing owner
        (v, r, ss) = signC(0xBEEF, c);
        vm.expectRevert(SeshRegistry.BadSignature.selector);
        reg.commit(s, c, v, r, ss);
        // penalty below the floor
        SeshRegistry.Commitment memory low = commitment(id, s, MIN_PEN - 1);
        (v, r, ss) = signC(makerPk, low);
        vm.expectRevert(SeshRegistry.BadParams.selector);
        reg.commit(s, low, v, r, ss);
        // expired
        (v, r, ss) = signC(makerPk, c);
        vm.warp(c.expiry + 1);
        vm.expectRevert(SeshRegistry.Expired.selector);
        reg.commit(s, c, v, r, ss);
    }

    function test_SiteListingCannotCommit() public {
        uint256 id = listSite();
        (XmrSwapV3.Swap memory s,) = lockEth(0.001 ether, bytes32(uint256(1)));
        SeshRegistry.Commitment memory c = commitment(id, s, MIN_PEN);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(makerPk, reg.commitmentDigest(c));
        vm.expectRevert(SeshRegistry.NotMaker.selector);
        reg.commit(s, c, v, r, ss);
    }

    function test_AckReleasesReserveAndBlocksSlash() public {
        uint256 lid = listMaker(MAKER_BOND);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth(0.001 ether, bytes32(uint256(1)));
        doCommit(lid, s, MIN_PEN);
        assertEq(reg.listing(lid).reserved, MIN_PEN);
        assertEq(reg.freeBond(lid), MAKER_BOND - MIN_PEN);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(makerPk, reg.ackDigest(id, lid));
        vm.prank(dave); // watchtower submits the maker's signed ack
        reg.ack(id, v, r, ss);
        assertEq(uint256(stage(id)), uint256(SeshRegistry.CommitStage.ACKED));
        assertEq(reg.listing(lid).reserved, 0);
        vm.warp(block.timestamp + ATTEST + 1);
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.slash(id);
    }

    function test_NoAckSlashesPenaltyToTaker() public {
        uint256 lid = listMaker(MAKER_BOND);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth(0.001 ether, bytes32(uint256(1)));
        doCommit(lid, s, MIN_PEN);
        vm.expectRevert(SeshRegistry.WrongTime.selector);
        reg.slash(id);
        vm.warp(block.timestamp + ATTEST);
        uint256 before = alice.balance;
        vm.prank(dave); // anyone
        reg.slash(id);
        assertEq(alice.balance - before, MIN_PEN);
        assertEq(reg.listing(lid).bond, MAKER_BOND - MIN_PEN);
        assertEq(reg.listing(lid).reserved, 0);
        assertEq(reg.listing(lid).openCommits, 0);
        // below the maker minimum now: no new swaps until topped up
        (XmrSwapV3.Swap memory s2,) = lockEth(0.001 ether, bytes32(uint256(2)));
        SeshRegistry.Commitment memory c = commitment(lid, s2, MIN_PEN);
        (uint8 v, bytes32 r, bytes32 ss) = signC(makerPk, c);
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.commit(s2, c, v, r, ss);
        assertEq(reg.maxQuoteWei(lid), 0);
        // the taker still gets the principal back through V3 as usual
    }

    function test_SlashStillAllowedAfterTakerCancels() public {
        uint256 lid = listMaker(MAKER_BOND);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth(0.001 ether, bytes32(uint256(1)));
        doCommit(lid, s, MIN_PEN);
        vm.warp(block.timestamp + ATTEST);
        vm.prank(alice);
        v3.cancelIntent(s);
        reg.slash(id);
        assertEq(uint256(stage(id)), uint256(SeshRegistry.CommitStage.SLASHED));
    }

    function test_ReadyOrClaimedReleasesWithoutAck() public {
        uint256 lid = listMaker(MAKER_BOND);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth(0.001 ether, bytes32(uint256(1)));
        doCommit(lid, s, MIN_PEN);
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.release(id);
        vm.prank(alice);
        v3.setReady(s); // taker saw the XMR
        vm.warp(block.timestamp + ATTEST + 1);
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.slash(id);
        reg.release(id);
        assertEq(reg.listing(lid).reserved, 0);
        assertEq(reg.listing(lid).bond, MAKER_BOND);
    }

    function test_AttestMustFitBeforeT1() public {
        uint256 lid = listMaker(MAKER_BOND);
        (XmrSwapV3.Swap memory s,) = lockEth(0.001 ether, bytes32(uint256(1)));
        vm.warp(s.t1 - ATTEST);
        SeshRegistry.Commitment memory c = commitment(lid, s, MIN_PEN);
        (uint8 v, bytes32 r, bytes32 ss) = signC(makerPk, c);
        vm.expectRevert(SeshRegistry.BadParams.selector);
        reg.commit(s, c, v, r, ss);
    }

    function test_OneCommitPerSwap() public {
        uint256 lid = listMaker(MAKER_BOND);
        (XmrSwapV3.Swap memory s,) = lockEth(0.001 ether, bytes32(uint256(1)));
        doCommit(lid, s, MIN_PEN);
        SeshRegistry.Commitment memory c = commitment(lid, s, MIN_PEN);
        (uint8 v, bytes32 r, bytes32 ss) = signC(makerPk, c);
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.commit(s, c, v, r, ss);
    }

    // ------------------------------------------------------------ exit delay
    function test_WithdrawOnlyAfterDelayAndSlashableMeanwhile() public {
        uint256 lid = listMaker(MAKER_BOND);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth(0.001 ether, bytes32(uint256(1)));
        doCommit(lid, s, MIN_PEN);
        vm.prank(maker);
        reg.requestExit(lid);
        assertEq(reg.maxQuoteWei(lid), 0); // hidden: no new swaps
        vm.prank(maker);
        vm.expectRevert(SeshRegistry.WrongTime.selector);
        reg.withdraw(lid, maker);
        // still slashable during the delay
        vm.warp(block.timestamp + ATTEST);
        reg.slash(id);
        vm.warp(block.timestamp + EXIT_DELAY);
        uint256 before = maker.balance;
        vm.prank(maker);
        reg.withdraw(lid, maker);
        assertEq(maker.balance - before, MAKER_BOND - MIN_PEN);
        assertEq(uint256(reg.listing(lid).status), uint256(SeshRegistry.Status.WITHDRAWN));
        // the name is free again
        assertEq(reg.idOfSesh(keccak256(bytes(MSESH))), 0);
        vm.prank(carol);
        reg.register{value: SITE_BOND}(MSESH, "site", "");
    }

    function test_WithdrawWaitsForOpenCommit() public {
        uint256 lid = listMaker(MAKER_BOND);
        (XmrSwapV3.Swap memory s,) = lockEth(0.001 ether, bytes32(uint256(1)));
        doCommit(lid, s, MIN_PEN);
        vm.prank(maker);
        reg.requestExit(lid);
        vm.warp(block.timestamp + EXIT_DELAY);
        vm.prank(maker);
        vm.expectRevert(SeshRegistry.WrongTime.selector);
        reg.withdraw(lid, maker);
    }

    function test_CancelExit() public {
        uint256 lid = listSite();
        vm.startPrank(carol);
        reg.requestExit(lid);
        reg.cancelExit(lid);
        vm.stopPrank();
        assertEq(uint256(reg.listing(lid).status), uint256(SeshRegistry.Status.ACTIVE));
    }

    // ------------------------------------------------------------ challenges
    function test_UnansweredChallengeRemovesListingAndPaysChallenger() public {
        uint256 lid = listSite();
        vm.prank(dave);
        vm.expectRevert(SeshRegistry.BadParams.selector);
        reg.challenge{value: CH_BOND - 1}(lid);
        vm.prank(dave);
        reg.challenge{value: CH_BOND}(lid);
        vm.expectRevert(SeshRegistry.WrongTime.selector);
        reg.resolveChallenge(lid);
        vm.warp(block.timestamp + CH_WINDOW);
        uint256 before = dave.balance;
        reg.resolveChallenge(lid);
        assertEq(dave.balance - before, SITE_BOND + CH_BOND);
        assertEq(uint256(reg.listing(lid).status), uint256(SeshRegistry.Status.REMOVED));
        assertEq(reg.idOfSesh(keccak256(bytes("carolsforum.sesh"))), 0);
        // cannot be resolved twice
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.resolveChallenge(lid);
    }

    function test_AnsweredChallengePaysOwner() public {
        uint256 lid = listSite();
        vm.prank(dave);
        reg.challenge{value: CH_BOND}(lid);
        vm.prank(dave);
        vm.expectRevert(SeshRegistry.Challenged_.selector);
        reg.challenge{value: CH_BOND}(lid);
        uint256 before = carol.balance;
        vm.prank(carol);
        reg.answer(lid);
        assertEq(carol.balance - before, CH_BOND);
        assertEq(reg.listing(lid).challenger, address(0));
        vm.warp(block.timestamp + CH_WINDOW);
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.resolveChallenge(lid);
    }

    function test_LateAnswerTooLate() public {
        uint256 lid = listSite();
        vm.prank(dave);
        reg.challenge{value: CH_BOND}(lid);
        vm.warp(block.timestamp + CH_WINDOW);
        vm.prank(carol);
        vm.expectRevert(SeshRegistry.WrongTime.selector);
        reg.answer(lid);
    }

    function test_ChallengeDuringExitBlocksWithdraw() public {
        uint256 lid = listSite();
        vm.prank(carol);
        reg.requestExit(lid);
        vm.prank(dave);
        reg.challenge{value: CH_BOND}(lid);
        vm.warp(block.timestamp + EXIT_DELAY);
        vm.prank(carol);
        vm.expectRevert(SeshRegistry.Challenged_.selector);
        reg.withdraw(lid, carol);
        // unanswered: the challenger takes it
        reg.resolveChallenge(lid);
        assertEq(reg.listing(lid).bond, 0);
    }

    function test_RemovedMakerKeepsReserveForTaker() public {
        uint256 lid = listMaker(MAKER_BOND);
        (XmrSwapV3.Swap memory s, bytes32 id) = lockEth(0.001 ether, bytes32(uint256(1)));
        doCommit(lid, s, MIN_PEN);
        vm.prank(dave);
        reg.challenge{value: CH_BOND}(lid);
        vm.warp(block.timestamp + CH_WINDOW);
        uint256 d0 = dave.balance;
        reg.resolveChallenge(lid);
        assertEq(dave.balance - d0, MAKER_BOND - MIN_PEN + CH_BOND);
        assertEq(reg.listing(lid).bond, MIN_PEN);
        uint256 a0 = alice.balance;
        reg.slash(id); // attest window long past
        assertEq(alice.balance - a0, MIN_PEN);
        vm.expectRevert(SeshRegistry.WrongStatus.selector);
        reg.sweepRemoved(lid); // nothing left
    }

    function test_RejectingRecipientIsOwedNotStuck() public {
        RejectEth rj = new RejectEth();
        vm.deal(address(rj), 1 ether);
        uint256 lid = rj.registerOn{value: SITE_BOND}(reg, "rejector.sesh");
        rj.exit(reg, lid);
        vm.warp(block.timestamp + EXIT_DELAY);
        rj.withdrawTo(reg, lid, address(rj));
        assertEq(reg.owed(address(rj)), SITE_BOND);
    }

    function test_NoAdmin() public view {
        // There is nothing to call: no owner(), pause(), setParams() or upgrade.
        assertEq(reg.siteMinBond(), SITE_BOND);
        assertEq(reg.makerMinBond(), MAKER_BOND);
    }

    function testFuzz_CapBoundary(uint96 bond, uint96 value) public {
        bond = uint96(bound(bond, MAKER_BOND, 1 ether));
        value = uint96(bound(value, 1e12, 10 ether));
        vm.deal(maker, 2 ether);
        uint256 lid = listMaker(bond);
        vm.deal(alice, 20 ether);
        (XmrSwapV3.Swap memory s,) = lockEth(value, bytes32(uint256(7)));
        uint256 pen = reg.requiredPenalty(address(0), value);
        SeshRegistry.Commitment memory c = commitment(lid, s, pen);
        (uint8 v, bytes32 r, bytes32 ss) = signC(makerPk, c);
        if (uint256(value) > uint256(bond) * CAP) {
            vm.expectRevert(SeshRegistry.OverCap.selector);
            reg.commit(s, c, v, r, ss);
        } else if (pen > bond) {
            vm.expectRevert(SeshRegistry.LowBond.selector);
            reg.commit(s, c, v, r, ss);
        } else {
            reg.commit(s, c, v, r, ss);
            assertEq(reg.listing(lid).reserved, pen);
        }
    }
}
